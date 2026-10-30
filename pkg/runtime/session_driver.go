package runtime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"weak"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

// sessionStartGate is consulted before a driver starts a run; a non-nil error
// aborts the start (see SetPreStartErrorGate).
type sessionStartGate func() error

type retryableSessionError struct{ error }

func retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableSessionError{err}
}

func isRetryableSessionError(err error) bool {
	if _, ok := errors.AsType[retryableSessionError](err); ok {
		return true
	}
	if sessionErr, ok := errors.AsType[*SessionError](err); ok && sessionErr.Kind == SessionErrorCapacity {
		return true
	}
	return session.IsTemporary(err)
}

type driverWorkGroup struct {
	mu    sync.Mutex
	count int
	done  chan struct{}
}

func newDriverWorkGroup() *driverWorkGroup {
	done := make(chan struct{})
	close(done)
	return &driverWorkGroup{done: done}
}

func (g *driverWorkGroup) Add(delta int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if delta > 0 && g.count == 0 {
		g.done = make(chan struct{})
	}
	g.count += delta
	if g.count < 0 {
		panic("runtime: negative session driver work count")
	}
	if g.count == 0 && delta < 0 {
		close(g.done)
	}
}

func (g *driverWorkGroup) Done() { g.Add(-1) }

func (g *driverWorkGroup) DoneChan() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done
}

func (g *driverWorkGroup) Wait() { <-g.DoneChan() }

// sessionDriver is one actor: one owner goroutine and mailbox per session. It is the only place
// that decides whether detached input is buffered into a live run, wakes an
// idle session, attaches a caller to an in-flight run, or cancels a
// runtime-owned wake run.

type sessionDriver struct {
	sessionLifecycle

	withdrawnInputs   map[string]string
	inputFingerprints map[string]string

	r  *LocalRuntime
	wg *driverWorkGroup

	mu             sync.Mutex
	sess           *session.Session
	registryState  atomic.Pointer[driverRegistryState]
	ownerCommands  chan *sessionOwnerCommand
	ownerStop      chan struct{}
	ownerClose     sync.Once
	ownerDone      chan struct{}
	treeProjection atomic.Pointer[subagent.Snapshot]
	treeWake       chan struct{}
	stopRequest    atomic.Pointer[driverStopRequest]
	stopWake       chan struct{}
	retirementGate chan struct{}
	// Stable admission topology; readable without waiting for per-session I/O.
	resourceOwner           *tools.ResourceOwner
	teamEnvironment         *TeamEnvironmentLease
	runEnvironment          *TeamEnvironmentLease
	teamEditStopPending     bool
	teamEditAdoptOnNextTurn bool
	identityID              string
	identityParent          string
	identityAsync           bool

	viewDormant       bool
	stoppedView       bool
	explicitStopped   bool
	causalReportOnly  bool
	causalReports     map[string]session.ChildReport
	causalReportOrder []string
	completionReport  *session.ChildReport

	completionErr                  error
	persistenceFailure             error
	reportRetry                    *session.ChildReport
	completionRunErr               string
	completionInFlight             bool
	backgroundWorks                int
	deferredCompletionReportTurnID string
	backgroundWorkCancel           map[uint64]map[string]func()
	backgroundWorkCancelRequested  map[uint64]bool

	startDone     chan struct{}
	startCanceled bool

	reclaiming         bool
	maintenanceRetired bool // protected by registry.mu; fences worker reservations on removal

	// retirement is the claimed driver's single background retirement. Protected by registry.mu.
	retirement *driverRetirement
	// supersession is the single retirement of this stopped generation, run before
	// a publication of the same ID replaces it. Protected by registry.mu.
	supersession *driverRetirement

	// lastRetireErr is the most recent failed retirement of this generation, reclaim or
	// supersession, kept so a later timeout can report its cause. Protected by registry.mu.
	lastRetireErr error

	retryRunning          bool
	turnChanged           chan struct{}
	completedTurns        []string
	consumedSteering      []string
	recentRetries         map[string]bool
	pending               []QueuedMessage
	deferredAccepted      map[string]deferredAcceptedInput
	deferredAcceptedOrder []string
	interruptRequested    bool
	steeringChanged       chan struct{}

	compactReserved      bool
	queuedCompaction     *liveCompactionRequest
	ioLane               *sessionIOLane
	ioReservations       int
	ioSealed             bool
	resourceCleanup      *resourceCleanupAttempt
	editReserved         bool
	durableStopRequested bool
	compactCancel        context.CancelFunc
	compactOperation     uint64
	switchReserved       bool
	skillOperationID     string
	skillCancel          context.CancelFunc
	skillGeneration      uint64
	modelRef             string
	modelProviders       []provider.Provider
	harnessEffort        harnessEffortOverride
	bindingVersion       uint64
	pauseCh              chan struct{}
	pauseGeneration      uint64
	pausePublished       uint64
	interactions         map[string]sessionInteraction
	events               *sessionEventHub
	lastError            string
	// unidentifiedUncertain is set when a write for a queue head without a
	// request ID failed ambiguously. Automatic wakes and promotion stop until an
	// explicit retry is admitted; a write that actually committed may be repeated.
	unidentifiedUncertain bool
	startUnidentifiedHead *unidentifiedStartHead // ID-less head reserved by the current generation's start
	lastFailureKey        string
	lastActive            time.Time
	preStart              sessionStartGate
	beforePrepareStart    func()
	// beforeAnyPrepareStart is a test seam parked by every start (post, wake,
	// scheduler, drive) before the active request is claimed. Nil in production.
	beforeAnyPrepareStart atomic.Pointer[func()]
	abortStart            func()
	onStarted             map[int]func()
	onSettled             map[int]func()
	nextHookID            int
	settled               chan struct{}
	titleStatus           string
	backgroundProcesses   map[string]tools.BackgroundProcess
	titleGeneration       uint64
	titleCancel           context.CancelFunc
	titleWriteReserved    bool
}

type sessionInteraction struct {
	kind       InteractionKind
	turnID     string
	generation uint64

	event  Event
	waiter *elicitationWaiter
	resume chan ResumeRequest
}

// admitLocked centralizes lifecycle admission for driver and handle operations.
// Callers must hold d.mu and remain responsible for operation-specific mailbox
// commits and for decorating request-scoped errors.
func (d *sessionDriver) admitLocked(op SessionOperation) *SessionError {
	sessionID := d.sessionIDLocked()
	stopped := func(reason SessionErrorReason) *SessionError {
		return &SessionError{Kind: SessionErrorStopped, SessionID: sessionID, Operation: op, Reason: reason}
	}
	busy := func(operation SessionOperation) *SessionError {
		return &SessionError{Kind: SessionErrorCapacity, SessionID: sessionID, Operation: operation, Reason: SessionErrorReasonBusy}
	}

	request := d.stopRequest.Load()
	if d.stopped || (request != nil && !request.applied.Load()) {
		reason := SessionErrorReason("")
		if op == SessionOperationRunSkill || op == SessionOperationSwitchAgent {
			reason = SessionErrorReasonBusy
		}
		return stopped(reason)
	}

	if d.persistenceFailure != nil && !session.IsTemporary(d.persistenceFailure) {
		return &SessionError{Kind: SessionErrorPersistence, SessionID: sessionID, Operation: op, Detail: "session writes are blocked: " + d.persistenceFailure.Error()}
	}
	switch op {
	case SessionOperationPost, SessionOperationSteer, SessionOperationPause:
		if d.skillOperationID != "" {
			return busy(SessionOperationSkillBusy)
		}
		if op == SessionOperationPost && d.switchReserved {
			return busy(SessionOperationSwitchAgent)
		}
	case SessionOperationCompact:
		if d.compactReserved || d.starting() || d.settling() || d.reclaiming || d.switchReserved || d.skillOperationID != "" {
			err := busy(SessionOperationCompactBusy)
			err.Limit = d.pendingLimit()
			return err
		}
		if d.running() && len(d.pending) != 0 {
			return &SessionError{Kind: SessionErrorCapacity, SessionID: sessionID, Operation: SessionOperationCompactPending, Reason: SessionErrorReasonPending, Limit: d.pendingLimit()}
		}
		// Off the dormant view, user steering (unset origin included) blocks idle
		// compaction; agent and runtime guidance never does.
		if !d.viewDormant && slices.ContainsFunc(d.pending, func(item QueuedMessage) bool {
			return item.InputMode == "steer" && normalizedInputOrigin(item.InputOrigin) == session.InputOriginUser
		}) {
			return &SessionError{Kind: SessionErrorCapacity, SessionID: sessionID, Operation: SessionOperationCompactPending, Reason: SessionErrorReasonPending, Limit: d.pendingLimit()}
		}
	case SessionOperationRunSkill:
		if d.skillOperationID != "" || d.running() || d.starting() || d.settling() || d.reclaiming || d.compactReserved || d.switchReserved || d.pauseCh != nil || len(d.pending) != 0 {
			return busy(op)
		}
	case SessionOperationSwitchAgent:
		if d.skillOperationID != "" || d.switchReserved || d.running() || d.starting() || d.settling() || d.reclaiming || d.compactReserved || d.pauseCh != nil || len(d.pending) != 0 || len(d.interactions) != 0 {
			return busy(op)
		}
	}
	return nil
}

func newSessionDriver(r *LocalRuntime, sess *session.Session) *sessionDriver {
	settled := make(chan struct{})
	close(settled)
	d := &sessionDriver{resourceOwner: tools.NewResourceOwner(), ownerCommands: make(chan *sessionOwnerCommand, 64), ownerStop: make(chan struct{}), ownerDone: make(chan struct{}), treeWake: make(chan struct{}, 1), stopWake: make(chan struct{}, 1), retirementGate: make(chan struct{}, 1), r: r, turnChanged: make(chan struct{}), wg: newDriverWorkGroup(), sess: sess, events: newSessionEventHubWithLimits(r.maxReplayEvents, r.maxReplayBytes), settled: settled, lastActive: time.Now(), interactions: map[string]sessionInteraction{}, onStarted: map[int]func(){}, onSettled: map[int]func(){}}
	if sess != nil {
		d.identityID, d.identityParent, d.identityAsync = sess.ID, sess.ParentID, sess.AsyncSubagent
		for position, item := range sess.MessagesSnapshot() {
			if item.Message == nil || !item.Message.Pending || !item.Message.Accepted || sess.TurnOutcome(item.Message.TurnID) != "" {
				continue
			}
			queued := queuedSessionInput(item.Message, position, r.sessionStore != nil)
			if item.Message.InputMode == "legacy_run" {
				d.pending = append([]QueuedMessage{queued}, d.pending...)
			} else {
				d.pending = append(d.pending, queued)
			}
		}
	}
	d.publishRegistryStateLocked()
	go d.runOwner()
	return d
}

func (d *sessionDriver) Subscribe(buffer int) (seed []Event, events <-chan Event, cancel func()) {
	return d.events.Subscribe(d.sessionID(), buffer)
}

func (d *sessionDriver) Drive(ctx context.Context, sess *session.Session) <-chan Event {
	if sess == nil || sess.ID != d.identityID {
		out := make(chan Event)
		close(out)
		return out
	}
	if runCtx, generation, ok := d.tryStart(ctx); ok {
		out := make(chan Event, defaultEventChannelCapacity)
		go d.driveToOut(runCtx, generation, out)
		return out
	}
	return d.attachThenRun(ctx)
}

func (d *sessionDriver) startWake(ctx context.Context, generation uint64) {
	go func() {
		defer d.wg.Done()
		d.driveWake(ctx, generation)
	}()
}

func (d *sessionDriver) Done() <-chan struct{} {
	return d.wg.DoneChan()
}

func (d *sessionDriver) Wait() {
	d.wg.Wait()
}

func promotionFailureKey(turnID string, err error) string {
	kind := "unknown"
	if sessionErr, ok := errors.AsType[*SessionError](err); ok {
		kind = string(sessionErr.Kind) + ":" + string(sessionErr.Operation)
	} else if session.IsTemporary(err) {
		kind = "temporary"
	}
	return turnID + "|" + kind + "|" + err.Error()
}

func (d *sessionDriver) publishPromotionFailureLocked(turnID string, err error) {
	d.flushAllDeferredAcceptedLocked()
	key := promotionFailureKey(turnID, err)
	if d.lastFailureKey == key {
		return
	}
	d.lastFailureKey = key
	if turnID == "" {
		// An ID-less head has no request identity, so its failure is session-scoped.
		d.events.Publish(d.sessionIDLocked(), ErrorWithCodeForSession(d.sessionIDLocked(), "input_promotion_failed", err.Error()))
		return
	}
	d.events.PublishForRequest(d.sessionIDLocked(), turnID, ErrorForSession(d.sessionIDLocked(), err.Error()))
}

func (d *sessionDriver) PostSteer(ctx context.Context, msg QueuedMessage) bool {
	_, err := d.postSteer(ctx, msg)
	return err == nil
}

// postSteer admits guidance through the shared mailbox and starts an idle driver.
func (d *sessionDriver) postSteer(ctx context.Context, msg QueuedMessage) (bool, error) {
	msg.InputMode = "steer"
	return d.postWithOperation(ctx, msg, true, SessionOperationSteer)
}

// DrainSteering promotes accepted guidance before making it visible to the provider loop.
func (d *sessionDriver) DrainSteering() []QueuedMessage { return d.drainGuidance(d.r.lifetime()) }

func (d *sessionDriver) schedulePendingRetry() {
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		if !d.stopped && !d.viewDormant && !d.compactReserved && d.pendingWakeableLocked() {
			d.retryRunning = true
		}
		return nil
	})
	if d.r.sessionDrivers != nil {
		d.r.sessionDrivers.signalWork()
	}
}

// publishDeferredAcceptedLocked preserves the canonical admission-before-promotion
// sequence while classifying only an input whose durable promotion has already
// completed as immediate. The session owner lock must be held.
func (d *sessionDriver) publishDeferredAcceptedLocked(turnID string, immediate bool) {
	if turnID == "" || d.deferredAccepted == nil {
		return
	}
	index := slices.Index(d.deferredAcceptedOrder, turnID)
	if index < 0 {
		return
	}
	// A successful promotion is necessarily the FIFO head. If stale deferred
	// entries precede it, publish them conservatively before its transition.
	for _, prior := range slices.Clone(d.deferredAcceptedOrder[:index]) {
		d.publishOneDeferredAcceptedLocked(prior, false)
	}
	d.publishOneDeferredAcceptedLocked(turnID, immediate)
}

func (d *sessionDriver) publishOneDeferredAcceptedLocked(turnID string, immediate bool) {
	deferred, ok := d.deferredAccepted[turnID]
	if !ok {
		return
	}
	delete(d.deferredAccepted, turnID)
	d.deferredAcceptedOrder = slices.DeleteFunc(d.deferredAcceptedOrder, func(id string) bool { return id == turnID })
	deferred.event.Immediate = immediate
	d.events.PublishForRequest(d.sessionIDLocked(), turnID, deferred.event)
}

func (d *sessionDriver) flushAllDeferredAcceptedLocked() {
	for len(d.deferredAcceptedOrder) != 0 {
		d.publishOneDeferredAcceptedLocked(d.deferredAcceptedOrder[0], false)
	}
}

func (d *sessionDriver) flushDeferredAcceptedIfNotStarting() {
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		if d.starting() {
			return nil
		}
		d.flushAllDeferredAcceptedLocked()
		return nil
	})
}

func (d *sessionDriver) queuedAtAdmissionLocked(msg QueuedMessage) bool {
	return !msg.guidance() || d.compactReserved || !d.running() || d.activeRequestID == ""
}

// admittedQueued reports whether an accepted input was queued at its admission
// commit. Input without a request ID cannot be located in the pending FIFO
// afterwards, so the admission decision is its only record.
func (d *sessionDriver) admittedQueued(msg QueuedMessage, admission inputAdmission) bool {
	if msg.RequestID == "" {
		return admission.queued
	}
	return d.inputQueued(msg)
}

func (d *sessionDriver) inputQueued(msg QueuedMessage) bool {
	queued := false
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		if msg.RequestID == "" {
			return nil
		}
		for _, item := range d.pending {
			if item.RequestID == msg.RequestID {
				queued = d.queuedAtAdmissionLocked(item)
				return nil
			}
		}
		if d.sess != nil {
			for _, item := range d.sess.MessagesSnapshot() {
				if item.Message != nil && item.Message.TurnID == msg.RequestID {
					queued = item.Message.Pending
					break
				}
			}
		}
		return nil
	})
	return queued
}

func (d *sessionDriver) Post(ctx context.Context, msg QueuedMessage, wake bool) bool {
	_, err := d.post(ctx, msg, wake)
	return err == nil
}

// post returns queued when the accepted input remains in the pending FIFO at
// the admission commit point.
func (d *sessionDriver) post(ctx context.Context, msg QueuedMessage, wake bool) (bool, error) {
	return d.postWithOperation(ctx, msg, wake, SessionOperationPost)
}

func (d *sessionDriver) postWithOperation(ctx context.Context, msg QueuedMessage, wake bool, operation SessionOperation) (bool, error) {
	return d.postWithOperationAuthorization(ctx, msg, wake, operation, false)
}

func (d *sessionDriver) postManualSubmit(ctx context.Context, msg QueuedMessage) (bool, error) {
	return d.postWithOperationAuthorization(ctx, msg, true, SessionOperationPost, true)
}

func (d *sessionDriver) postManualSteer(ctx context.Context, msg QueuedMessage) (bool, error) {
	msg.InputMode = "steer"
	return d.postWithOperationAuthorization(ctx, msg, true, SessionOperationSteer, true)
}

func (d *sessionDriver) postWithOperationAuthorization(ctx context.Context, msg QueuedMessage, wake bool, operation SessionOperation, authorizeDormant bool) (queued bool, postErr error) {
	token := new(byte)
	var idleAdmission bool
	var before func()
	for {
		var starting bool
		var done chan struct{}
		if err := d.ownerCall(ctx, func() error {
			starting, done = d.starting() && !d.compactReserved && !d.stopped, d.startDone
			idleAdmission = !d.running() && !d.starting() && !d.settling()
			before = d.beforePrepareStart
			return nil
		}); err != nil {
			return false, err
		}
		if !starting {
			break
		}
		select {
		case <-done:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	var admission inputAdmission
	var err error
	if authorizeDormant {
		admission, err = d.admitInputReceiptWithAuthorization(ctx, msg, operation, wake, idleAdmission, token, true)
	} else {
		admission, err = d.admitInputForStart(ctx, msg, operation, wake, idleAdmission, token)
	}
	if err != nil {
		return false, err
	}
	if admission.deferredAccepted && !admission.startOwner {
		return d.admittedQueued(msg, admission), nil
	}
	deferredAccepted := admission.deferredAccepted
	d.refreshAttention()
	if idleAdmission && wake && before != nil {
		before()
	}
	var idle, canWake bool
	_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
		idle = !d.running() && !d.starting() && !d.settling()
		canWake = d.pendingWakeableLocked()
		return nil
	})
	woke := false
	if idle && wake && canWake {
		woke = true
		runCtx, generation, callbacks, startErr := d.prepareStart(d.r.lifetime(), true)
		if startErr == nil {
			for _, fn := range callbacks {
				fn()
			}
			d.startWake(runCtx, generation)
		} else {
			d.schedulePendingRetry()
			if deferredAccepted {
				d.flushDeferredAcceptedIfNotStarting()
			}
			if msg.Retry || msg.RequestID == "" {
				var stopped bool
				_ = d.ownerCall(context.WithoutCancel(ctx), func() error { stopped = d.stopped; return nil })
				if stopped && !d.inputQueued(msg) {
					return false, ErrSessionStopped
				}
			}
		}
	}
	// A retry admitted to the mailbox can be cleared by a stop before any wake
	// runs; it was never accepted into a run, so report the stop, not success.
	if msg.Retry && !woke {
		var stopped bool
		_ = d.ownerCall(context.WithoutCancel(ctx), func() error { stopped = d.stopped; return nil })
		if stopped && !d.inputQueued(msg) {
			return false, ErrSessionStopped
		}
	}
	queued = d.admittedQueued(msg, admission)
	if deferredAccepted {
		d.flushDeferredAcceptedIfNotStarting()
	}
	return queued, nil
}

// PostReliable appends a lifecycle note before attempting admission. Capacity
// denial is not delivery failure: the bounded pending mailbox retains the note
// for a later release signal. Stopped drivers and mailbox overflow still fail.
func (d *sessionDriver) PostReliable(ctx context.Context, msg QueuedMessage) bool {
	msg.InputOrigin = session.InputOriginRuntime
	msg.InputMode = "steer"
	_, err := d.post(ctx, msg, true)
	return err == nil
}

// WakePending attempts to start an idle driver only when retained input exists.
// It never appends input; admission denial leaves the mailbox untouched.
func (d *sessionDriver) WakePending() bool {
	err := d.wakePending()
	if err == nil {
		return true
	}
	if isRetryableSessionError(err) {
		d.schedulePendingRetry()
	}
	return false
}

func (d *sessionDriver) wakePending() error {
	d.mu.Lock()
	pending := d.pendingWakeableLocked()
	idle := !d.running() && !d.starting() && !d.settling() && !d.stopped && !d.viewDormant
	d.mu.Unlock()
	if !pending || !idle {
		return &SessionError{Kind: SessionErrorInvalid, SessionID: d.sessionID(), Operation: "wake_pending"}
	}
	wakeCtx, generation, callbacks, err := d.prepareStart(d.r.lifetime(), true)
	if err != nil {
		return err
	}
	for _, fn := range callbacks {
		fn()
	}
	d.startWake(wakeCtx, generation)
	return nil
}

func (d *sessionDriver) pendingLimit() int {
	if d.r != nil {
		return d.r.maxPendingMailbox
	}
	return 0
}

func (d *sessionDriver) DrainPending() []QueuedMessage {
	var pending []QueuedMessage
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error { pending = d.pending; d.pending = nil; d.refreshSteeringLocked(); return nil })
	return pending
}

func (d *sessionDriver) DrainRuntimeNotes() []QueuedMessage { return d.drainGuidance(d.r.lifetime()) }

func (d *sessionDriver) HasPending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pending) > 0
}

func (d *sessionDriver) SetPreStartGate(fn func() bool, abort func()) {
	gate := sessionStartGate(nil)
	if fn != nil {
		gate = func() error {
			if fn() {
				return nil
			}
			return &SessionError{Kind: SessionErrorCapacity, SessionID: d.sessionID(), Operation: "start_gate", Reason: SessionErrorReasonBusy}
		}
	}
	d.SetPreStartErrorGate(gate, abort)
}

func (d *sessionDriver) SetPreStartErrorGate(fn sessionStartGate, abort func()) {
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error { d.preStart = fn; d.abortStart = abort; return nil })
}

func (d *sessionDriver) OnStarted(fn func()) func() { return d.registerCallback(fn, true) }
func (d *sessionDriver) OnSettled(fn func()) func() { return d.registerCallback(fn, false) }
func (d *sessionDriver) registerCallback(fn func(), started bool) func() {
	id := 0
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		d.nextHookID++
		id = d.nextHookID
		if started {
			if d.onStarted == nil {
				d.onStarted = map[int]func(){}
			}
			d.onStarted[id] = fn
		} else {
			if d.onSettled == nil {
				d.onSettled = map[int]func(){}
			}
			d.onSettled[id] = fn
		}
		return nil
	})
	ref := weak.Make(d)
	return func() {
		if owner := ref.Value(); owner != nil {
			_ = owner.ownerCall(context.WithoutCancel(owner.r.lifetime()), func() error {
				if started {
					delete(owner.onStarted, id)
				} else {
					delete(owner.onSettled, id)
				}
				return nil
			})
		}
	}
}

// authorizeViewLocked is called only at successful explicit admission.
func (d *sessionDriver) authorizeViewLocked() bool {
	d.causalReportOnly = false
	d.causalReports = nil
	if !d.viewDormant {
		return false
	}
	d.viewDormant = false
	d.events.Publish(d.sessionIDLocked(), &DormancyChangedEvent{AgentContext: newAgentContext(d.AgentNameLocked()), Type: "session_dormancy_changed", SessionID: d.sessionIDLocked(), Dormant: false, Stopped: d.explicitStopLocked()})
	return true
}

func (d *sessionDriver) authorizeCausalReportViewLocked() bool {
	if !d.viewDormant {
		return false
	}
	d.viewDormant = false
	d.events.Publish(d.sessionIDLocked(), &DormancyChangedEvent{AgentContext: newAgentContext(d.AgentNameLocked()), Type: "session_dormancy_changed", SessionID: d.sessionIDLocked(), Dormant: false, Stopped: d.explicitStopLocked()})
	return true
}

func (d *sessionDriver) TogglePause(ctx context.Context) (bool, error) {
	paused := false
	err := d.ownerCall(ctx, func() error {
		if err := d.admitLocked(SessionOperationPause); err != nil {
			return err
		}
		d.pauseGeneration++
		if d.pauseCh != nil {
			close(d.pauseCh)
			d.pauseCh = nil
		} else {
			d.pauseCh = make(chan struct{})
			paused = true
		}
		d.events.Publish(d.identityID, PauseChanged(d.identityID, d.AgentNameLocked(), paused))
		if paused && d.skillOperationID == "" && !d.running() && !d.starting() && !d.settling() && len(d.pending) == 0 {
			d.pausePublished = d.pauseGeneration
			d.events.Publish(d.identityID, Paused(d.identityID, d.AgentNameLocked()))
		}
		return nil
	})
	return paused, err
}

func (d *sessionDriver) waitIfPaused(ctx context.Context, reached func()) (bool, error) {
	blocked := false
	for {
		var ch chan struct{}
		publish := false
		if err := d.ownerCall(ctx, func() error {
			ch = d.pauseCh
			publish = ch != nil && d.pausePublished != d.pauseGeneration
			if publish {
				d.pausePublished = d.pauseGeneration
			}
			return nil
		}); err != nil {
			return blocked, err
		}
		if ch == nil {
			return blocked, nil
		}
		blocked = true
		if publish && reached != nil {
			reached()
		}
		select {
		case <-ch:
			// Recheck under the lock: resume followed immediately by repause
			// must keep this generation at the boundary.
			continue
		case <-ctx.Done():
			return true, ctx.Err()
		}
	}
}

func (d *sessionDriver) cloneForAgentSwitch(ctx context.Context) (*session.Session, error) {
	var snapshot *session.Session
	err := d.ownerCall(ctx, func() error {
		if err := d.admitLocked(SessionOperationSwitchAgent); err != nil {
			return err
		}
		if d.sess == nil || d.identityParent != "" {
			return &SessionError{Kind: SessionErrorUnsupported, SessionID: d.identityID, Operation: "switch_agent", Reason: SessionErrorReasonBusy}
		}
		snapshot = d.sess.Clone()
		return nil
	})
	return snapshot, err
}

func (d *sessionDriver) ModelProvidersEmpty() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.modelProviders) == 0
}

func (d *sessionDriver) ModelRef() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.modelRef
}

func (d *sessionDriver) ModelSnapshot() (string, []provider.Provider) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.modelRef, slices.Clone(d.modelProviders)
}

type activeSessionBinding struct {
	version   uint64
	agentName string
	modelRef  string
	models    []provider.Provider
	harness   harnessEffortOverride
}

func (d *sessionDriver) activeBinding() activeSessionBinding {
	d.mu.Lock()
	defer d.mu.Unlock()
	return activeSessionBinding{d.bindingVersion, d.AgentNameLocked(), d.modelRef, slices.Clone(d.modelProviders), d.harnessEffort}
}

func (d *sessionDriver) validateBinding(ctx context.Context, binding activeSessionBinding) error {
	return d.ownerCall(ctx, func() error { return d.validateBindingLocked(binding) })
}

func (d *sessionDriver) validateBindingLocked(binding activeSessionBinding) error {
	if d.bindingVersion != binding.version || d.AgentNameLocked() != binding.agentName {
		return &SessionError{Kind: SessionErrorStale, SessionID: d.identityID, Operation: "active_binding"}
	}
	return nil
}

func (d *sessionDriver) ModelBindingSnapshot() (uint64, string, []provider.Provider) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bindingVersion, d.modelRef, slices.Clone(d.modelProviders)
}

// scopeModels pins this session's model binding on ctx so every consumer that
// resolves the agent's model through it — the run loop, compaction, context
// accounting and the agent/team info the sidebar renders — sees the same
// per-session override instead of the agent's YAML default.
func (d *sessionDriver) scopeModels(ctx context.Context) context.Context {
	ctx = d.scopeTeamEnvironment(ctx)
	binding := d.activeBinding()
	ctx = withHarnessEffortOverride(ctx, binding.harness)
	return agent.WithContextModels(ctx, binding.agentName, binding.models)
}

func (d *sessionDriver) SetModelBinding(modelRef string, providers []provider.Provider) {
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		d.modelRef = modelRef
		d.modelProviders = slices.Clone(providers)
		d.bindingVersion++
		return nil
	})
}

// setHarnessEffort records the session's effort for its bound harness agent. It
// is a session binding change, not a configuration change: the shared agent
// stays untouched and the binding version advances so cached projections refresh.
// The binding guard rejects a choice made against an agent the session no longer runs.
func (d *sessionDriver) setHarnessEffort(ctx context.Context, binding activeSessionBinding, level effort.Level) error {
	return d.ownerCall(ctx, func() error {
		if err := d.validateBindingLocked(binding); err != nil {
			return err
		}
		if d.stopped || d.sess == nil {
			return ErrSessionStopped
		}
		d.harnessEffort = harnessEffortOverride{agentName: binding.agentName, level: level, set: true}
		d.bindingVersion++
		return nil
	})
}

func (d *sessionDriver) SetModelOverride(ctx context.Context, agentName, modelRef string, providers []provider.Provider) error {
	return d.setModelOverride(ctx, d.activeBinding(), agentName, modelRef, providers)
}

func (d *sessionDriver) setModelOverride(ctx context.Context, binding activeSessionBinding, agentName, modelRef string, providers []provider.Provider) error {
	return d.durableIO(ctx, func() (sessionIOReservation, error) {
		if err := d.validateBindingLocked(binding); err != nil {
			return sessionIOReservation{}, err
		}
		if agentName != binding.agentName {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorStale, SessionID: d.identityID, Operation: "set_model"}
		}
		if d.stopped || d.sess == nil {
			return sessionIOReservation{}, ErrSessionStopped
		}
		next := d.sess.OwnSnapshot()
		next.SetAgentModelOverride(agentName, modelRef)
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if d.r.sessionStore != nil {
					return d.r.sessionStore.UpdateSession(ctx, next)
				}
				return nil
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				if err := d.validateBindingLocked(binding); err != nil {
					return err
				}
				d.sess.SetAgentModelOverride(agentName, modelRef)
				d.modelRef = modelRef
				d.modelProviders = slices.Clone(providers)
				d.bindingVersion++
				return nil
			},
		}, nil
	})
}

func (d *sessionDriver) SetStarred(ctx context.Context, starred bool) error {
	return d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped || d.sess == nil {
			return sessionIOReservation{}, ErrSessionStopped
		}
		id := d.identityID
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if d.r.sessionStore != nil {
					return d.r.sessionStore.SetSessionStarred(ctx, id, starred)
				}
				return nil
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				d.sess.Starred = starred
				return nil
			},
		}, nil
	})
}

func (d *sessionDriver) RemoveAttachment(ctx context.Context, path string) error {
	return d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped || d.sess == nil {
			return sessionIOReservation{}, ErrSessionStopped
		}
		next := d.sess.OwnSnapshot()
		if !next.RemoveAttachedFile(path) {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorNotFound, SessionID: d.identityID, Operation: "remove_attachment"}
		}
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if d.r.sessionStore != nil {
					return d.r.sessionStore.UpdateSession(ctx, next)
				}
				return nil
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				d.sess.RemoveAttachedFile(path)
				return nil
			},
		}, nil
	})
}

func (d *sessionDriver) UpdateTitle(ctx context.Context, title string) error {
	return d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped || d.sess == nil {
			return sessionIOReservation{}, ErrSessionStopped
		}
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if d.r.sessionStore != nil {
					return d.r.sessionStore.UpdateSessionTitle(ctx, d.identityID, title)
				}
				return nil
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				d.invalidateTitleLocked()
				d.sess.SetTitle(title)
				d.events.Publish(d.identityID, SessionTitle(d.identityID, title))
				return nil
			},
		}, nil
	})
}

type driverObservation struct {
	seed   []SequencedSessionEvent
	live   <-chan SessionEvent
	done   <-chan struct{}
	cancel func()

	cursor              uint64
	session             *session.Session
	status              SessionStatus
	interactions        []InteractionSnapshot
	pendingInputs       []PendingInput
	position            int
	models              []provider.Provider
	harness             harnessEffortOverride
	titleStatus         string
	backgroundProcesses []tools.BackgroundProcess
}

// beginReclaimLocked returns driverReclaimed once the driver has entered
// reclaim. Work still being scheduled is transient (driverReclaimBusy); any
// retained state makes the driver ineligible.
func (d *sessionDriver) beginReclaimLocked() driverReclaimOutcome {
	if len(d.withdrawnInputs) != 0 && d.r.sessionStore == nil {
		return driverReclaimIneligible
	}
	// Maintenance owns storage/report delivery even when no turn is executing.
	select {
	case <-d.wg.DoneChan():
	default:
		return driverReclaimBusy
	}

	if d.reclaiming || d.stopped || d.viewDormant || d.reportRetry != nil || d.running() || d.starting() || d.settling() || d.retryRunning || d.skillOperationID != "" || d.backgroundWorks != 0 || d.deferredCompletionReportTurnID != "" || len(d.pending) != 0 || len(d.interactions) != 0 || d.events.HasSubscribers(d.sessionIDLocked()) {
		return driverReclaimIneligible
	}
	d.reclaiming = true
	d.stopped = true
	d.skillGeneration++
	d.skillOperationID = ""
	d.skillCancel = nil
	return driverReclaimed
}

func (d *sessionDriver) observeTail(ctx context.Context, limits SessionHistoryLimits, buffer int) (driverObservation, SessionSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return driverObservation{}, SessionSnapshot{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reclaiming || (d.stopped && !d.stoppedView) || d.sess == nil {
		return driverObservation{}, SessionSnapshot{}, &SessionError{Kind: SessionErrorStopped, SessionID: d.sessionIDLocked(), Operation: "observe"}
	}
	seed, live, cancel, cursor, done := d.events.SubscribePublic(d.sessionIDLocked(), nil, buffer)
	if live == nil || cancel == nil {
		return driverObservation{}, SessionSnapshot{}, &SessionError{Kind: SessionErrorStopped, SessionID: d.sessionIDLocked(), Operation: "observe"}
	}
	position := d.sess.ItemCount()
	header, err := d.sess.HistoryHeaderSnapshot(limits.MaxBytes)
	if err != nil {
		cancel()
		return driverObservation{}, SessionSnapshot{}, err
	}
	status := d.statusLocked()
	interactions := make([]InteractionSnapshot, 0, len(d.interactions))
	for id, interaction := range d.interactions {
		interactions = append(interactions, InteractionSnapshot{SessionID: d.sessionIDLocked(), InteractionID: id, Kind: interaction.kind, Event: interaction.event})
	}
	pending := make([]PendingInput, 0, len(d.pending))
	for _, q := range d.pending {
		pending = append(pending, PendingInput{TurnID: q.ID, Content: q.Content, MultiContent: q.MultiContent, SessionPosition: q.AcceptedPosition, InputOrigin: q.InputOrigin, SenderID: q.SenderID, SenderName: q.SenderName, ReportOutcome: q.ReportOutcome, InputMode: q.InputMode})
	}
	epoch, _ := d.events.CurrentCursor(d.sessionIDLocked())
	historyRevision := d.sess.HistoryRevisionSnapshot()
	base := SessionSnapshot{Projection: "tail", Session: header, Status: status, BackgroundProcesses: d.backgroundProcessesLocked(), Interactions: interactions, PendingInputs: pending, Epoch: epoch, Cursor: cursor, TranscriptPosition: position, TitleStatus: d.titleStatus}
	if !historyPageFits(base, limits.MaxBytes) {
		cancel()
		return driverObservation{}, SessionSnapshot{}, ErrHistoryMetadataTooLarge
	}
	page, err := d.sess.ReadBoundedItemsWithBudget(position, limits.MaxItems, limits.MaxItemBytes, func(candidate session.BoundedItemPage) bool {
		candidate.TotalItems = position
		candidate.BeforePosition = position
		view := base
		view.Items = candidate.Items
		view.History = &candidate
		if candidate.HasMore {
			view.HistoryCursor = encodeHistoryCursor(historyCursor{Version: 1, SessionID: d.sessionIDLocked(), Epoch: epoch, AnchorCursor: cursor, AnchorPosition: position, BeforePosition: candidate.NextBeforePosition, HistoryRevision: historyRevision})
		}
		return historyPageFits(view, limits.MaxBytes)
	})
	if err != nil {
		cancel()
		if errors.Is(err, session.ErrHistoryPageEnvelopeTooLarge) {
			return driverObservation{}, SessionSnapshot{}, ErrHistoryMetadataTooLarge
		}
		return driverObservation{}, SessionSnapshot{}, err
	}
	page.TotalItems = position
	page.BeforePosition = position
	snapshot := base
	snapshot.Items = page.Items
	snapshot.History = &page
	if page.HasMore {
		snapshot.HistoryCursor = encodeHistoryCursor(historyCursor{Version: 1, SessionID: d.sessionIDLocked(), Epoch: epoch, AnchorCursor: cursor, AnchorPosition: position, BeforePosition: page.NextBeforePosition, HistoryRevision: historyRevision})
	}
	if !historyPageFits(snapshot, limits.MaxBytes) {
		cancel()
		return driverObservation{}, SessionSnapshot{}, ErrHistoryMetadataTooLarge
	}

	for i := range snapshot.PendingInputs {
		snapshot.PendingInputs[i].MultiContent = session.CloneMessageParts(snapshot.PendingInputs[i].MultiContent)
	}
	for i := range snapshot.Interactions {
		if detached, e := observerEventSnapshot(snapshot.Interactions[i].Event); e == nil {
			snapshot.Interactions[i].Event = detached
		} else {
			cancel()
			return driverObservation{}, SessionSnapshot{}, ErrHistoryMetadataTooLarge
		}
	}
	if page.HasMore {
		snapshot.HistoryCursor = encodeHistoryCursor(historyCursor{Version: 1, SessionID: d.sessionIDLocked(), Epoch: epoch, AnchorCursor: cursor, AnchorPosition: position, BeforePosition: page.NextBeforePosition, HistoryRevision: historyRevision})
	}
	replay := make([]SessionEvent, len(seed))
	for i, item := range seed {
		replay[i] = sessionEnvelope(d.sessionIDLocked(), item)
	}
	obs := driverObservation{seed: seed, live: live, done: done, cancel: cancel, cursor: cursor, session: header, status: status, interactions: interactions, pendingInputs: pending, position: position, harness: d.harnessEffort, titleStatus: d.titleStatus, backgroundProcesses: slices.Clone(base.BackgroundProcesses)}
	return obs, snapshot, nil
}

func (d *sessionDriver) observe(since *uint64, buffer int) driverObservation {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reclaiming || (d.stopped && !d.stoppedView) {
		return driverObservation{}
	}
	seed, live, cancel, cursor, done := d.events.SubscribePublic(d.sessionIDLocked(), since, buffer)
	cloned, status, interactions, pendingInputs, position := d.snapshotLocked()
	processes := d.backgroundProcessesLocked()
	for i := range seed {
		if event, ok := seed[i].Event.(*BackgroundProcessesEvent); ok {
			event.Processes = slices.Clone(processes)
		}
	}
	if since != nil {
		// Same-epoch clients preserve their projection on reconnect, so append an
		// authoritative sequence-zero replacement after replay (including empty state).
		lifecycle := &BackgroundProcessesEvent{AgentContext: newAgentContext(d.AgentNameLocked()), Type: "background_processes", SessionID: d.sessionIDLocked(), Processes: slices.Clone(processes)}
		bytes := estimateEventBytes(lifecycle)
		replayBytes := bytes
		gap := len(seed) >= maxSessionEventSubscriberBuffer
		for _, item := range seed {
			replayBytes += estimateEventBytes(item.Event) + len(item.RequestID) + len(item.InteractionID)
			if item.Gap {
				gap = true
			}
		}
		if replayBytes > maxSessionEventSubscriberBytes {
			gap = true
		}
		if gap {
			seed = []SequencedSessionEvent{{Epoch: d.events.epoch, Gap: true, FirstAvailable: cursor + 1}}
		} else {
			seed = append(seed, SequencedSessionEvent{Epoch: d.events.epoch, Event: lifecycle})
		}
	}
	return driverObservation{seed: seed, live: live, done: done, cancel: cancel, cursor: cursor, session: cloned, status: status, interactions: interactions, pendingInputs: pendingInputs, position: position, models: slices.Clone(d.modelProviders), harness: d.harnessEffort, titleStatus: d.titleStatus, backgroundProcesses: processes}
}

func (d *sessionDriver) Cancel(turnID string) CancelOutcome {
	outcome := CancelNotActive
	var cancel context.CancelFunc
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		if !d.running() || d.cancel == nil || turnID == "" || d.activeRequestID != turnID {
			return nil
		}
		if d.cancelling() {
			outcome = CancelAlreadyCancelling
			return nil
		}
		d.phase = sessionCancelling
		d.invalidateTitleLocked()
		d.resolveInteractionsLocked()
		cancel = d.cancel
		outcome = CancelAccepted
		return nil
	})
	if cancel != nil {
		cancel()
	}
	return outcome
}

func (d *sessionDriver) StopAll() bool {
	return d.stopAll(false)
}

func (d *sessionDriver) StopAllForDelete() bool {
	return d.stopAll(true)
}

// stopAllRetiring stops execution as part of driver removal; its dormancy is not
// observable (see applyStopLocked). The wait ends with ctx or with owner closure, so
// a retirement can never hold the retirement gate past its bound.
func (d *sessionDriver) stopAllRetiring(ctx context.Context) bool {
	active := d.requestStopIntent(false, false, false, true)
	d.cancelAllBackgroundWorks()
	_ = d.awaitStop(ctx)
	return active
}

func (d *sessionDriver) stopAll(deleting bool) bool {
	return d.stopAllWithin(context.WithoutCancel(d.r.lifetime()), deleting)
}

// stopAllWithin requests a stop and waits no longer than ctx or owner closure.
func (d *sessionDriver) stopAllWithin(ctx context.Context, deleting bool) bool {
	active := d.requestStop(deleting)
	d.cancelAllBackgroundWorks()
	_ = d.awaitStop(ctx)
	return active
}

func (d *sessionDriver) isStopped() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped
}

// stoppedForObservation reports a stop that is final for observers. Unlike a
// reclaiming session, which refuses a live stream only for now, it never resumes
// a stream, so a tree observer may treat it as settled.
func (d *sessionDriver) stoppedForObservation() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped && !d.stoppedView && !d.reclaiming
}

func (d *sessionDriver) stoppedAndSettled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped && !d.running() && !d.starting() && !d.settling()
}

func (d *sessionDriver) Settled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.running() && !d.starting() && !d.settling() && d.skillOperationID == "" && len(d.pending) == 0
}

func (d *sessionDriver) ActiveRequestID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.activeRequestID
}

func (d *sessionDriver) refreshAttention() {
	if d.r == nil || d.r.subagents == nil {
		return
	}
	d.mu.Lock()
	waitingOn := ""
	for _, interaction := range d.interactions {
		if interaction.kind == InteractionConfirmation || interaction.kind == InteractionMaxIterations {
			waitingOn = "approve tool"
			break
		}
		waitingOn = "answer question"
	}
	d.mu.Unlock()
	d.r.subagents.setAttention(d.sessionID(), waitingOn)
}

func (d *sessionDriver) RegisterInteraction(requestID string, kind InteractionKind, event ...Event) {
	if requestID == "" {
		return
	}
	var payload Event
	if len(event) != 0 {
		payload = event[0]
		if detached, err := observerEventSnapshot(payload); err == nil {
			payload = detached
		} else {
			return
		}
	}
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		interaction := d.interactions[requestID]
		if interaction.generation != 0 && (interaction.generation != d.generation || interaction.turnID != d.activeRequestID) {
			return nil
		}
		interaction.kind, interaction.turnID, interaction.generation, interaction.event = kind, d.activeRequestID, d.generation, payload
		d.interactions[requestID] = interaction
		return nil
	})
	if d.r != nil && d.r.subagents != nil {
		d.refreshAttention()
	}
}

func (d *sessionDriver) Respond(response InteractionResponse) error {
	if response.InteractionID == "" {
		return &SessionError{Kind: SessionErrorInvalid, SessionID: d.sessionID(), Operation: "respond"}
	}
	err := d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		id := d.sessionIDLocked()
		fail := func(kind SessionErrorKind, operation string) error {
			return &SessionError{Kind: kind, SessionID: id, RequestID: response.InteractionID, Operation: SessionOperation(operation)}
		}
		interaction, pending := d.interactions[response.InteractionID]
		if !pending || d.stopped || interaction.turnID != d.activeRequestID || interaction.generation != d.generation {
			return fail(SessionErrorStale, "respond_generation")
		}
		if interaction.kind != response.Kind {
			return fail(SessionErrorInvalid, "respond_kind")
		}
		if d.r == nil {
			return fail(SessionErrorNotFound, "respond")
		}
		var delivered bool
		switch response.Kind {
		case InteractionConfirmation, InteractionMaxIterations:
			if !IsValidResumeType(response.Resume.Type) {
				return fail(SessionErrorInvalid, "respond_resume")
			}
			response.Resume.Type = NormalizeResumeType(response.Resume.Type)
			response.Resume.SessionID = id
			response.Resume.RequestID = response.InteractionID
			if interaction.resume != nil {
				select {
				case interaction.resume <- response.Resume:
					delivered = true
				default:
				}
			}
		case InteractionElicitation:
			event, ok := interaction.event.(*ElicitationRequestEvent)
			if !ok || response.ElicitationID == "" || event.ElicitationID != response.ElicitationID {
				return fail(SessionErrorInvalid, "respond_elicitation")
			}
			if interaction.waiter != nil {
				delivered = interaction.waiter.tryResolve(response.Elicitation)
			}
		default:
			return fail(SessionErrorInvalid, "respond")
		}
		if !delivered {
			return fail(SessionErrorStale, "respond")
		}
		// Claim and channel delivery are one transition, even if the receiver drains immediately.
		d.resolveInteractionLocked(response.InteractionID)
		return nil
	})
	d.refreshAttention()
	return err
}

func (d *sessionDriver) LastError() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastError
}

func (d *sessionDriver) snapshotLocked() (*session.Session, SessionStatus, []InteractionSnapshot, []PendingInput, int) {
	var cloned *session.Session
	position := 0
	pendingInputs := make([]PendingInput, 0, len(d.pending))
	if d.sess != nil {
		cloned = d.sess.Clone()
		input, output := cloned.Usage()
		cloned.SetTokensAndCost(input, output, cloned.TotalCost())
		position = len(cloned.Messages)
		for index, item := range cloned.Messages {
			if item.Message == nil || !item.Message.Pending || !item.Message.Accepted {
				continue
			}
			pendingInputs = append(pendingInputs, PendingInput{TurnID: item.Message.TurnID, Content: item.Message.Message.Content, MultiContent: item.Message.Message.MultiContent, SessionPosition: index, InputOrigin: item.Message.InputOrigin, SenderID: item.Message.SenderID, SenderName: item.Message.SenderName, ReportOutcome: item.Message.ReportOutcome, InputMode: item.Message.InputMode})
		}
	}
	interactions := make([]InteractionSnapshot, 0, len(d.interactions))
	for requestID, interaction := range d.interactions {
		payload := interaction.event
		if detached, err := observerEventSnapshot(payload); err == nil {
			payload = detached
		}
		item := InteractionSnapshot{SessionID: d.sessionIDLocked(), InteractionID: requestID, Kind: interaction.kind, Event: payload}
		if elicitation, ok := interaction.event.(*ElicitationRequestEvent); ok {
			item.ElicitationID = elicitation.ElicitationID
		}
		interactions = append(interactions, item)
	}
	return cloned, d.statusLocked(), interactions, pendingInputs, position
}

func (d *sessionDriver) explicitStopLocked() bool {
	request := d.stopRequest.Load()
	return d.explicitStopped || d.stoppedView || d.durableStopRequested || request != nil && (request.withdraw.Load() || request.explicitStopped.Load())
}

const (
	backgroundProcessRetention   = 4 * time.Second
	maxRecentBackgroundProcesses = 64
)

func (d *sessionDriver) pruneBackgroundProcessesLocked(now time.Time) {
	for id, process := range d.backgroundProcesses {
		if process.State != tools.BackgroundProcessRunning && !process.FinishedAt.IsZero() && now.Sub(process.FinishedAt) >= backgroundProcessRetention {
			delete(d.backgroundProcesses, id)
		}
	}
}

func (d *sessionDriver) backgroundProcessesLocked() []tools.BackgroundProcess {
	d.pruneBackgroundProcessesLocked(time.Now())
	out := make([]tools.BackgroundProcess, 0, len(d.backgroundProcesses))
	for _, process := range d.backgroundProcesses {
		out = append(out, process)
	}
	slices.SortFunc(out, func(a, b tools.BackgroundProcess) int {
		if order := a.StartedAt.Compare(b.StartedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

func (d *sessionDriver) publishBackgroundProcessesLocked() {
	processes := d.backgroundProcessesLocked()
	d.events.Publish(d.sessionIDLocked(), &BackgroundProcessesEvent{AgentContext: newAgentContext(d.AgentNameLocked()), Type: "background_processes", SessionID: d.sessionIDLocked(), Processes: processes})
}

func (d *sessionDriver) adoptBackgroundProcess(ctx context.Context, process tools.BackgroundProcess) error {
	err := d.ownerCall(context.WithoutCancel(ctx), func() error {
		if d.stopped || d.sess == nil || process.SessionID != d.sessionIDLocked() || process.ID == "" || process.State != tools.BackgroundProcessRunning || process.StartedAt.IsZero() || !process.FinishedAt.IsZero() {
			return ErrSessionClosed
		}
		if d.backgroundProcesses == nil {
			d.backgroundProcesses = make(map[string]tools.BackgroundProcess)
		}
		if _, exists := d.backgroundProcesses[process.ID]; exists {
			return errors.New("duplicate background process identity")
		}
		d.pruneBackgroundProcessesLocked(time.Now())
		for len(d.backgroundProcesses) >= maxRecentBackgroundProcesses {
			var oldestID string
			var oldest time.Time
			for id, current := range d.backgroundProcesses {
				if current.State != tools.BackgroundProcessRunning && (oldestID == "" || current.FinishedAt.Before(oldest)) {
					oldestID, oldest = id, current.FinishedAt
				}
			}
			if oldestID == "" {
				return errors.New("background process capacity exceeded")
			}
			delete(d.backgroundProcesses, oldestID)
		}
		process.Command = boundedProcessText(process.Command)
		process.Cwd = boundedProcessText(process.Cwd)
		if process.AgentName == "" {
			process.AgentName = d.AgentNameLocked()
		}
		d.backgroundProcesses[process.ID] = process
		d.publishBackgroundProcessesLocked()
		return nil
	})
	if err == nil {
		d.projectBackgroundProcessCount()
	}
	return err
}

func (d *sessionDriver) finishBackgroundProcess(ctx context.Context, id string, state tools.BackgroundProcessState, finishedAt time.Time) {
	if state != tools.BackgroundProcessCompleted && state != tools.BackgroundProcessStopped && state != tools.BackgroundProcessFailed {
		return
	}
	changed := false
	if err := d.ownerCall(context.WithoutCancel(ctx), func() error {
		process, ok := d.backgroundProcesses[id]
		if !ok || process.State != tools.BackgroundProcessRunning {
			return nil
		}
		process.State, process.FinishedAt = state, finishedAt
		d.backgroundProcesses[id] = process
		d.publishBackgroundProcessesLocked()
		changed = true
		return nil
	}); err == nil && changed {
		d.projectBackgroundProcessCount()
	}
}

func (d *sessionDriver) projectBackgroundProcessCount() {
	if d.r == nil || d.r.subagents == nil {
		return
	}
	d.r.subagents.updateBackgroundProcessCount(d)
}

func boundedProcessText(value string) string {
	const maxBytes = 4096
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (d *sessionDriver) statusLocked() SessionStatus {
	state := SessionStateSettled
	switch {
	case d.skillOperationID != "":
		state = SessionStateRunning
	case d.cancelling():
		state = SessionStateCancelling
	case d.running() || d.settling():
		state = SessionStateRunning
	case d.starting() || len(d.pending) != 0:
		state = SessionStateQueued
	}
	status := SessionStatus{
		InterruptedTurns: d.interruptedTurnsLocked(),
		State:            state, Pending: len(d.pending), TurnID: d.activeRequestID, LastError: d.lastError, Dormant: d.viewDormant, Stopped: d.explicitStopLocked(),
		PauseArmed: d.pauseCh != nil, Paused: d.pauseCh != nil && (d.pausePublished == d.pauseGeneration || (!d.running() && !d.starting())), PauseGeneration: d.pauseGeneration,
	}
	if d.sess != nil {
		status.SessionID = d.sess.ID
		status.AgentName = d.sess.AgentName
	}
	return status
}

func (d *sessionDriver) Status() SessionStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.statusLocked()
}

// compact serializes manual compaction with this session's execution. Running
// sessions use the existing live-session boundary queue. A settled session is
// claimed for the duration of the standalone compaction so submissions cannot
// start against the same mutable session snapshot. Accepted session input is a
// separate state: idle compaction summarizes only history, retaining the FIFO
// unchanged behind the reservation until compaction releases it.
func (d *sessionDriver) compact(ctx context.Context, additionalPrompt string, sink EventSink) error {
	var operation uint64
	var running bool
	var scratch *session.Session
	var operationCtx context.Context
	var cancel context.CancelFunc
	var operationEnv *TeamEnvironmentLease
	err := d.ownerCall(ctx, func() error {
		if err := d.admitLocked(SessionOperationCompact); err != nil {
			return err
		}
		d.compactReserved = true
		d.compactOperation++
		operation = d.compactOperation
		running = d.running()
		if running {
			if sink == nil {
				sink = EventSinkFunc(func(Event) {})
			}
			d.queuedCompaction = &liveCompactionRequest{additionalPrompt: additionalPrompt, events: sink}
		}
		if !running {
			scratch = d.sess.Clone()
			operationCtx, cancel = context.WithCancel(ctx) //nolint:gosec,fatcontext // owner retains cancel; worker defers it and stop fences it
			d.compactCancel = cancel
			d.phase = sessionStarting
			d.startDone = make(chan struct{})
			d.lastActive = time.Now()
			d.openSettledLocked()
			d.wg.Add(1)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if sink == nil {
		sink = EventSinkFunc(func(Event) {})
	}
	if running {
		return nil
	}
	if d.r.teamEnvironments != nil {
		d.mu.Lock()
		pinned := d.teamEnvironment
		d.mu.Unlock()
		if pinned == nil {
			_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
				d.compactReserved = false
				d.leave(sessionStarting)
				d.signalStartDoneLocked()
				d.closeSettledLocked()
				return nil
			})
			cancel()
			d.wg.Done()
			return errors.New("session environment is not pinned")
		}
		var acquireErr error
		operationEnv, acquireErr = d.r.teamEnvironments.Acquire(pinned.Environment().RevisionID())
		if acquireErr != nil {
			_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
				d.compactReserved = false
				d.leave(sessionStarting)
				d.signalStartDoneLocked()
				d.closeSettledLocked()
				return nil
			})
			cancel()
			d.wg.Done()
			return acquireErr
		}
	}
	go func() {
		defer d.wg.Done()
		defer cancel()
		if operationEnv != nil {
			defer func() { _ = operationEnv.Release(context.WithoutCancel(operationCtx)) }()
		}
		workerCtx := context.WithValue(operationCtx, compactionIdentityKey{}, compactionIdentity{d, operation})
		if operationEnv != nil {
			workerCtx = context.WithValue(workerCtx, teamEnvironmentContextKey{}, operationEnv.Environment())
		}
		d.r.runLiveCompactionRequest(d.scopeModels(workerCtx), scratch, liveCompactionRequest{additionalPrompt: additionalPrompt, events: sink})
		var callbacks []func()
		wake := false
		_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
			if d.compactOperation != operation {
				return nil
			}
			d.compactReserved = false
			d.compactCancel = nil
			d.leave(sessionStarting)
			d.lastActive = time.Now()
			d.signalStartDoneLocked()
			d.closeSettledLocked()
			d.refreshSteeringLocked()
			wake = !d.stopped && d.pendingWakeableLocked()
			callbacks = d.settledCallbacksLocked()
			return nil
		})
		for _, fn := range callbacks {
			fn()
		}
		if wake {
			d.WakePending()
		}
	}()
	return nil
}

func (d *sessionDriver) attachThenRun(ctx context.Context) <-chan Event {
	out := make(chan Event, defaultEventChannelCapacity)
	go func() {
		defer close(out)
		emit := func(e Event) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}

		seed, events, cancel := d.Subscribe(defaultEventChannelCapacity)
		defer cancel()
		seenLiveStop := false
		for _, e := range seed {
			if !emit(e) {
				return
			}
		}

		drainEvents := func(events <-chan Event) bool {
			for {
				select {
				case e, ok := <-events:
					if !ok || !emit(e) {
						return false
					}
				default:
					return true
				}
			}
		}

		for {
			d.mu.Lock()
			running := d.running()
			stopped := d.stopped

			settled := d.settled
			d.mu.Unlock()
			if !running {
				if !drainEvents(events) {
					return
				}
				if stopped {
					return
				}
				if runCtx, generation, ok := d.tryStart(ctx); ok {
					d.driveToOutNoClose(runCtx, generation, out)
					d.wg.Done()
				}
				return
			}

			select {
			case <-ctx.Done():
				return
			case e, ok := <-events:
				if !ok || !emit(e) {
					return
				}
				if _, stopped := e.(*StreamStoppedEvent); stopped {
					seenLiveStop = true
				}
			case <-settled:
				if !drainEvents(events) {
					return
				}
				if !seenLiveStop && !emit(StreamStopped(d.sessionID(), d.session().AgentName, "normal")) {
					return
				}
				if runCtx, generation, ok := d.tryStart(ctx); ok {
					d.driveToOutNoClose(runCtx, generation, out)
					d.wg.Done()
				}
				return
			}
		}
	}()
	return out
}

func (d *sessionDriver) tryStart(ctx context.Context) (context.Context, uint64, bool) {
	runCtx, generation, callbacks, err := d.prepareStart(ctx, false)
	if err != nil {
		return nil, 0, false
	}
	for _, fn := range callbacks {
		fn()
	}
	return runCtx, generation, true
}

// prepareStart performs the manager admission handshake without holding d.mu,
// then atomically promotes the oldest accepted mailbox item before publishing
// a running generation. Promotion failure leaves the FIFO intact and no
// provider call can begin.
func (d *sessionDriver) prepareStart(ctx context.Context, wake bool) (context.Context, uint64, []func(), error) {
	if hook := d.beforeAnyPrepareStart.Load(); hook != nil {
		(*hook)()
	}
	return d.prepareStartReservation(ctx, wake, false)
}

func (d *sessionDriver) prepareStartAdmissionLocked(ctx context.Context, wake bool) (context.Context, uint64, []func(), error) {
	return d.prepareStartReservation(ctx, wake, false)
}

var errSessionAdmissionContended = errors.New("session admission is contended")

func (d *sessionDriver) prepareStartReservation(ctx context.Context, wake, continuation bool) (context.Context, uint64, []func(), error) {
	var gate sessionStartGate
	var abort func()
	var startDone chan struct{}
	var startGeneration uint64
	var err error
	for {
		err = d.ownerCall(ctx, func() error {
			if d.r.subagents != nil {
				if err := d.r.subagents.sessionAdmissionError(d.identityID); err != nil {
					return err
				}
			}
			if d.viewDormant {
				return &SessionError{Kind: SessionErrorInvalid, Operation: "view_dormant"}
			}
			if d.stopped || d.r.lifetime().Err() != nil {
				return ErrSessionStopped
			}
			if (!continuation && (d.running() || d.starting() || d.settling())) || d.switchReserved || d.compactReserved {
				return &SessionError{Kind: SessionErrorCapacity, Operation: SessionOperationStart, Reason: SessionErrorReasonBusy}
			}
			if !continuation && d.r.sessionDrivers != nil {
				if !d.r.sessionDrivers.runMu.TryLock() {
					return errSessionAdmissionContended
				}
				defer d.r.sessionDrivers.runMu.Unlock()
				if err := d.r.sessionDrivers.admitRun(d); err != nil {
					return err
				}
			}
			if continuation {
				if !d.starting() || d.startDone == nil {
					return &SessionError{Kind: SessionErrorCapacity, Operation: SessionOperationStart, Reason: SessionErrorReasonBusy}
				}
			} else {
				d.phase, d.startDone = sessionStarting, make(chan struct{})
				d.startCanceled = false
			}
			startDone = d.startDone
			startGeneration = d.generation
			if len(d.pending) > 0 {
				d.activeRequestID = d.pending[0].RequestID
			}
			d.publishRegistryStateLocked()
			if !continuation {
				gate, abort = d.preStart, d.abortStart
			}
			return nil
		})
		if !errors.Is(err, errSessionAdmissionContended) {
			break
		}
		timer := time.NewTimer(subagentPersistenceRetryBase)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, 0, nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, 0, nil, err
	}
	fail := func(err error) (context.Context, uint64, []func(), error) {
		if d.finishCanceledStartFor(startGeneration, startDone) {
			return nil, 0, nil, context.Canceled
		}
		var callbacks []func()
		var retry, canceled bool
		_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
			if continuation && (d.generation != startGeneration || d.startDone != startDone || (!d.starting() && d.phase != sessionIdle)) {
				return nil
			}
			if d.startCanceled {
				canceled = true
				return nil
			}
			if len(d.pending) > 0 {
				d.lastError = err.Error()
				d.publishPromotionFailureLocked(d.pending[0].RequestID, err)
			}
			d.leave(sessionStarting)
			d.activeRequestID = ""
			d.startCanceled = false
			d.signalStartDoneLocked()
			if continuation {
				d.closeSettledLocked()
				callbacks = d.settledCallbacksLocked()
				retry = true
			}
			return nil
		})
		if canceled {
			d.finishCanceledStartFor(startGeneration, startDone)
			return nil, 0, nil, context.Canceled
		}
		if abort != nil {
			abort()
		}
		for _, fn := range callbacks {
			fn()
		}
		if retry {
			d.schedulePendingRetry()
		}
		return nil, 0, nil, err
	}
	if d.r.teamEnvironments != nil && !continuation {
		if err := d.pinCurrentTeamEnvironment(ctx); err != nil {
			return fail(err)
		}
	}
	if gate != nil {
		if err := gate(); err != nil {
			if d.finishCanceledStartFor(startGeneration, startDone) {
				return nil, 0, nil, context.Canceled
			}
			return fail(err)
		}
	}
	var next QueuedMessage
	var startHead *QueuedMessage
	err = d.ownerCall(ctx, func() error {
		if d.stopped || !d.starting() {
			return ErrSessionStopped
		}
		if wake && len(d.pending) == 0 {
			return &SessionError{Kind: SessionErrorInvalid, Operation: SessionOperationWakePending}
		}
		if len(d.pending) > 0 {
			next = d.pending[0]
			if next.RequestID == "" && !next.guidance() {
				head := next
				startHead = &head
			}
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	if next.RequestID != "" {
		if err := d.promoteInput(ctx, next.RequestID, func(msg QueuedMessage) {
			d.pending = slices.DeleteFunc(d.pending, func(value QueuedMessage) bool { return value.RequestID == msg.RequestID })
			d.activeRequestID = msg.RequestID
		}); err != nil {
			if d.finishCanceledStartFor(startGeneration, startDone) {
				return nil, 0, nil, context.Canceled
			}
			return fail(err)
		}
	}
	if d.finishCanceledStartFor(startGeneration, startDone) {
		return nil, 0, nil, context.Canceled
	}
	var runCtx context.Context
	var generation uint64
	var callbacks []func()
	var runEnv *TeamEnvironmentLease
	if d.r.teamEnvironments != nil {
		var pinned *TeamEnvironmentLease
		if err := d.ownerCall(ctx, func() error { pinned = d.teamEnvironment; return nil }); err != nil {
			return fail(err)
		}
		if pinned == nil {
			return fail(errors.New("session environment is not pinned"))
		}
		var acquireErr error
		runEnv, acquireErr = d.r.teamEnvironments.Acquire(pinned.Environment().RevisionID())
		if acquireErr != nil {
			return fail(acquireErr)
		}
	}
	var previousRunEnv *TeamEnvironmentLease
	err = d.ownerCall(ctx, func() error {
		if d.r.subagents != nil {
			if err := d.r.subagents.sessionAdmissionError(d.identityID); err != nil {
				return err
			}
		}
		if d.stopped || !d.starting() {
			return ErrSessionStopped
		}
		if d.startCanceled {
			return context.Canceled
		}
		d.events.SetRequest(d.identityID, d.activeRequestID, d.generation+1)
		d.beginRun()
		d.generationResult, d.lastActive = "", time.Now()
		d.openSettledLocked()
		if !continuation {
			d.wg.Add(1)
		}
		d.generation++
		generation = d.generation
		// Only a start whose head is ID-less reserves one, and only for its own
		// generation; an ID'd or empty start binds none, so a later ID-less post
		// waits for the successor instead of being absorbed by this run.
		d.startUnidentifiedHead = nil
		if startHead != nil {
			d.startUnidentifiedHead = &unidentifiedStartHead{generation: generation, head: *startHead}
		}
		// A successor run replaces the lease its predecessor never released;
		// that lease is released below, outside the owner.
		previousRunEnv, d.runEnvironment = d.runEnvironment, runEnv
		var cancel context.CancelFunc
		runCtx, cancel = context.WithCancel(ctx) //nolint:fatcontext // owner transfers generation cancellation to execution worker
		stopLifetime := context.AfterFunc(d.r.lifetime(), cancel)
		d.cancel = func() { stopLifetime(); cancel() }
		if !continuation {
			callbacks = d.startedCallbacksLocked()
		}
		d.signalStartDoneLocked()
		return nil
	})
	if err != nil {
		if runEnv != nil {
			_ = runEnv.Release(context.WithoutCancel(ctx))
		}
		if d.finishCanceledStartFor(startGeneration, startDone) {
			return nil, 0, nil, context.Canceled
		}
		return fail(err)
	}
	if previousRunEnv != nil {
		_ = previousRunEnv.Release(context.WithoutCancel(ctx))
	}
	return runCtx, generation, callbacks, nil
}

func (d *sessionDriver) driveToOut(ctx context.Context, generation uint64, out chan<- Event) {
	defer d.wg.Done()
	defer close(out)
	d.driveToOutNoClose(ctx, generation, out)
}

func (d *sessionDriver) driveToOutNoClose(ctx context.Context, generation uint64, out chan<- Event) {
	for {
		nextCtx, nextGeneration, again := d.driveToOutGeneration(ctx, generation, out)
		if !again {
			return
		}
		ctx, generation = nextCtx, nextGeneration //nolint:fatcontext // successor context is independently rooted in supervisor lifetime
	}
}

func (d *sessionDriver) driveToOutGeneration(ctx context.Context, generation uint64, out chan<- Event) (context.Context, uint64, bool) {
	var runErr string
	cancelled := false
	// Promote before the execution snapshot is cloned so the model sees the head.
	promotedInput, skipModel, err := d.promoteUnidentifiedHead(ctx, generation)
	if err != nil {
		return d.finishRunContextOpts(d.r.durabilityContext(), generation, err.Error(), false)
	}
	if skipModel {
		return d.finishRunContextOpts(d.r.durabilityContext(), generation, "", true)
	}
	if promotedInput != nil {
		ctx = withPromotedRunInput(ctx, promotedInput)
	}
	ctx, scratch, err := d.executionContext(ctx, generation)
	if err != nil {
		return d.finishRun(generation, err.Error())
	}
	release, err := d.r.acquireExecution(ctx, d.identityID)
	if err != nil {
		return d.finishRun(generation, err.Error())
	}
	defer release()
	ctx, cancelBudget := d.r.executionBudgetContext(ctx, scratch, d.r.resolveSessionAgent(scratch))
	defer cancelBudget()
	run := d.r.runStreamRaw(ctx, scratch)
	for event := range run {
		if errEvent, ok := event.(*ErrorEvent); ok && runErr == "" {
			runErr = errEvent.Error
		}
		if cancelled {
			continue
		}
		select {
		case out <- event:
		default:
			select {
			case out <- event:
			case <-ctx.Done():
				cancelled = true
			}
		}
	}
	return d.finishRun(generation, runErr)
}

func (d *sessionDriver) driveWake(ctx context.Context, generation uint64) {
	for {
		nextCtx, nextGeneration, again := d.driveWakeGeneration(ctx, generation)
		if !again {
			return
		}
		ctx, generation = nextCtx, nextGeneration //nolint:fatcontext // successor context is independently rooted in supervisor lifetime
	}
}

func (d *sessionDriver) driveWakeGeneration(ctx context.Context, generation uint64) (context.Context, uint64, bool) {
	var runErr string
	// Promote before the execution snapshot is cloned so the model sees the head.
	promotedInput, skipModel, err := d.promoteUnidentifiedHead(ctx, generation)
	if err != nil {
		return d.finishRunContextOpts(d.r.durabilityContext(), generation, err.Error(), false)
	}
	if skipModel {
		return d.finishRunContextOpts(d.r.durabilityContext(), generation, "", true)
	}
	if promotedInput != nil {
		ctx = withPromotedRunInput(ctx, promotedInput)
	}
	ctx, scratch, err := d.executionContext(ctx, generation)
	if err != nil {
		return d.finishRun(generation, err.Error())
	}
	release, err := d.r.acquireExecution(ctx, d.identityID)
	if err != nil {
		return d.finishRun(generation, err.Error())
	}
	defer release()
	ctx, cancelBudget := d.r.executionBudgetContext(ctx, scratch, d.r.resolveSessionAgent(scratch))
	defer cancelBudget()
	for event := range d.r.runStreamRaw(ctx, scratch) {
		if errEvent, ok := event.(*ErrorEvent); ok && runErr == "" {
			runErr = errEvent.Error
		}
		// Drained for flow control only; observers and the session event hub
		// publish the wake run to subscribers.
	}
	return d.finishRun(generation, runErr)
}

func (d *sessionDriver) authorizeCompletionReportAfterSettlement(turnID string) {
	var report *session.ChildReport
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		if d.activeRequestID == turnID && d.completionInFlight && d.completionReport != nil && d.completionReport.TurnID == turnID {
			reportCopy := *d.completionReport
			report = &reportCopy
		}
		return nil
	})
	if report == nil {
		return
	}
	if d.r.subagents.authorizeExecutedTurnReport(context.WithoutCancel(d.r.lifetime()), d, turnID, *report) {
		_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
			if d.completionReport != nil && d.completionReport.ID == report.ID {
				d.completionReport = nil
			}
			return nil
		})
	}
}

func (d *sessionDriver) finishRun(generation uint64, runErr string) (context.Context, uint64, bool) {
	return d.finishRunContext(d.r.durabilityContext(), generation, runErr)
}

func (d *sessionDriver) finishRunContext(ctx context.Context, generation uint64, runErr string) (context.Context, uint64, bool) {
	return d.finishRunContextOpts(ctx, generation, runErr, true)
}

// finishRunContextOpts settles a generation. allowSuccessor is false when run
// start failed before any model call, so queued input is not re-attempted in a
// tight loop; an uncertain head also never starts a successor.
func (d *sessionDriver) finishRunContextOpts(ctx context.Context, generation uint64, runErr string, allowSuccessor bool) (context.Context, uint64, bool) {
	var turnID string
	var hasConsumedInputs bool
	var outcome TurnOutcome
	var continuation context.Context
	var previousCancel context.CancelFunc
	proceed := false
	err := d.ownerCall(ctx, func() error {
		if generation != d.generation || generation <= d.settledGeneration || d.completionInFlight {
			return nil
		}
		if runErr == "" && !d.cancelling() && !d.stopped && !d.settling() && d.hasGuidanceLocked() {
			previousCancel = d.cancel
			continuation, d.cancel = context.WithCancel(d.r.lifetime()) //nolint:fatcontext // independently rooted successor generation
			return nil
		}
		proceed = true
		d.lastError, d.completionInFlight = runErr, true
		d.clearStartUnidentifiedHeadLocked(generation)
		if !d.settling() {
			d.canceledOutcome = d.cancelling()
		}
		d.phase = sessionSettling
		turnID = d.activeRequestID
		hasConsumedInputs = len(d.consumedSteering) != 0
		outcome = TurnCompleted
		if d.canceledOutcome || d.stopped || d.r.lifetime().Err() != nil {
			outcome = TurnCanceled
		} else if runErr != "" {
			outcome = TurnFailed
		}
		return nil
	})
	if err != nil || !proceed {
		if continuation != nil {
			if previousCancel != nil {
				previousCancel()
			}
			return continuation, generation, true
		}
		return nil, 0, false
	}
	var completionErr error
	for _, observer := range d.r.observers {
		if p, ok := observer.(*PersistenceObserver); ok {
			if err := p.completionErrorContext(ctx, d.identityID); err != nil {
				completionErr = err
				break
			}
		}
	}
	if completionErr == nil && d.r.subagents != nil {
		completionErr = d.r.subagents.completeExecutedSessionTurnContext(ctx, d, turnID, runErr)
	}
	if completionErr == nil && (turnID != "" || hasConsumedInputs) {
		completionErr = d.persistTurnOutcome(ctx, turnID, outcome)
	}
	if completionErr == nil && turnID != "" && d.r.subagents != nil {
		d.authorizeCompletionReportAfterSettlement(turnID)
	}
	var completedCancel context.CancelFunc
	var firstCompletionFailure bool
	var callbacks []func()
	var successor bool
	var successorStartDone chan struct{}
	var teamEditStop bool
	_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
		d.completionInFlight = false
		if completionErr != nil {
			firstCompletionFailure = d.completionErr == nil
			d.completionErr, d.completionRunErr = completionErr, runErr
			d.lastError, d.persistenceFailure = completionErr.Error(), completionErr
			d.publishPersistenceFailureLocked(completionErr)
			d.notifyTurnChangedLocked()
			return nil
		}
		completedCancel = d.cancel
		teamEditStop = d.teamEditStopPending
		d.teamEditStopPending = false
		d.phase = sessionIdle
		d.settledGeneration, d.completionErr, d.persistenceFailure, d.lastFailureKey = generation, nil, nil, ""
		d.resolveInteractionsLocked()
		if turnID != "" {
			d.events.PublishForRequest(d.identityID, turnID, &TurnSettledEvent{AgentContext: newAgentContext(d.AgentNameLocked()), Type: "turn_settled", SessionID: d.identityID, TurnID: turnID, Outcome: outcome})
		}
		d.completeTurnLocked(turnID)
		successor = allowSuccessor && !d.unidentifiedUncertain && !teamEditStop && !d.stopped && d.r.lifetime().Err() == nil && len(d.pending) > 0
		if successor {
			d.phase, d.startDone = sessionStarting, make(chan struct{})
			successorStartDone = d.startDone
			d.startCanceled = false
		}
		d.cancel, d.activeRequestID = nil, ""
		d.events.SetRequest(d.identityID, "", 0)
		if !successor {
			d.closeSettledLocked()
			callbacks = d.settledCallbacksLocked()
		}
		return nil
	})
	if completedCancel != nil {
		completedCancel()
	}
	for _, fn := range callbacks {
		fn()
	}
	if completionErr != nil {
		if firstCompletionFailure && d.r.sessionDrivers != nil {
			d.r.sessionDrivers.signalWork()
		}
		return nil, 0, false
	}
	if !successor {
		d.releaseRunEnvironment(ctx, generation)
	}
	if successor {
		nextCtx, nextGeneration, started, err := d.prepareStartReservation(d.r.lifetime(), true, true)
		if err == nil {
			// The existing execution worker carries the successor generation.
			for _, fn := range started {
				fn()
			}
			return nextCtx, nextGeneration, true
		}
		var resetSuccessor, cancelSuccessor bool
		_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
			if d.generation != generation || !d.starting() || d.startDone != successorStartDone {
				return nil
			}
			if d.startCanceled {
				cancelSuccessor = true
				return nil
			}
			d.leave(sessionStarting)
			d.activeRequestID = ""
			d.signalStartDoneLocked()
			d.closeSettledLocked()
			callbacks = d.settledCallbacksLocked()
			resetSuccessor = true
			return nil
		})
		if cancelSuccessor {
			d.finishCanceledStartFor(generation, successorStartDone)
		}
		for _, fn := range callbacks {
			fn()
		}
		if resetSuccessor {
			d.schedulePendingRetry()
		}
	}
	if d.r.sessionDrivers != nil {
		d.r.sessionDrivers.signalWork()
	}
	return nil, 0, false
}

func (d *sessionDriver) signalStartDoneLocked() {
	if d.startDone == nil {
		return
	}
	select {
	case <-d.startDone:
	default:
		close(d.startDone)
	}
}

func (d *sessionDriver) startedCallbacksLocked() []func() {
	if len(d.onStarted) == 0 {
		return nil
	}
	callbacks := make([]func(), 0, len(d.onStarted))
	for _, fn := range d.onStarted {
		callbacks = append(callbacks, fn)
	}
	return callbacks
}

func (d *sessionDriver) settledCallbacksLocked() []func() {
	if len(d.onSettled) == 0 {
		return nil
	}
	callbacks := make([]func(), 0, len(d.onSettled))
	for _, fn := range d.onSettled {
		callbacks = append(callbacks, fn)
	}
	return callbacks
}

func (d *sessionDriver) replaceSession(sess *session.Session) error {
	if sess == nil {
		return ErrSessionClosed
	}
	next := sess.Clone()
	return d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		if d.stopped || d.ioSealed {
			return ErrSessionStopped
		}
		if d.running() || d.starting() || d.settling() || len(d.pending) != 0 || d.backgroundWorks != 0 || d.deferredCompletionReportTurnID != "" || d.ioReservations != 0 || d.compactReserved || d.editReserved || d.switchReserved || d.skillOperationID != "" || d.retryRunning || d.completionInFlight || len(d.interactions) != 0 || d.pauseCh != nil {
			return &SessionError{Kind: SessionErrorConflict, SessionID: d.identityID, Operation: "replace_settled_session"}
		}
		if next.ID != d.identityID || next.ParentID != d.identityParent || next.AsyncSubagent != d.identityAsync || d.sess == nil || next.AgentName != d.sess.AgentName || next.AgentID != d.sess.AgentID || next.AttributesSnapshot()[SessionAgentAttribute] != d.sess.AttributesSnapshot()[SessionAgentAttribute] {
			return &SessionError{Kind: SessionErrorInvalid, SessionID: d.identityID, Operation: "replace_pinned_session"}
		}
		if next.AgentModelOverrides[next.AgentName] != d.sess.AgentModelOverrides[d.sess.AgentName] {
			return &SessionError{Kind: SessionErrorConflict, SessionID: d.identityID, Operation: "replace_pinned_session"}
		}
		d.invalidateTitleLocked()
		next.MarkHistoryChanged()
		d.sess = next
		d.bindingVersion++
		return nil
	})
}

func (d *sessionDriver) AgentNameLocked() string {
	if d.sess == nil {
		return ""
	}
	return d.sess.AgentName
}

func (d *sessionDriver) AgentName() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.AgentNameLocked()
}

func (d *sessionDriver) session() *session.Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == nil {
		return nil
	}
	return d.sess.Clone()
}

func (d *sessionDriver) sessionIDLocked() string {
	if d.sess == nil {
		return ""
	}
	return d.sess.ID
}

func (d *sessionDriver) sessionID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessionIDLocked()
}

func (d *sessionDriver) openSettledLocked() {
	select {
	case <-d.settled:
		d.settled = make(chan struct{})
	default:
	}
}

func (d *sessionDriver) closeSettledLocked() {
	select {
	case <-d.settled:
	default:
		close(d.settled)
	}
}

func (d *sessionDriver) existingInputLocked(msg QueuedMessage) (bool, bool, error) {
	if msg.RequestID == "" || d.sess == nil {
		return false, false, nil
	}
	if _, withdrawn := d.withdrawnInputs[msg.RequestID]; withdrawn {
		return false, false, &SessionError{Kind: SessionErrorConflict, SessionID: d.identityID, RequestID: msg.RequestID, Operation: "submit", Detail: "input identity was withdrawn"}
	}
	for _, queued := range d.pending {
		if queued.RequestID != msg.RequestID {
			continue
		}
		if normalizedInputOrigin(queued.InputOrigin) != normalizedInputOrigin(msg.InputOrigin) || queued.SenderID != msg.SenderID || queued.SenderName != msg.SenderName || queued.ReportOutcome != msg.ReportOutcome || inputMode(queued.InputMode) != inputMode(msg.InputMode) || queued.Retry != msg.Retry || queued.Content != msg.Content || !reflect.DeepEqual(queued.MultiContent, msg.MultiContent) {
			return false, false, &SessionError{Kind: SessionErrorConflict, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "submit"}
		}
		return true, d.queuedAtAdmissionLocked(queued), nil
	}
	if d.recentRetries[msg.RequestID] {
		if !msg.Retry {
			return false, false, &SessionError{Kind: SessionErrorConflict, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "submit"}
		}
		return true, false, nil
	}

	for _, item := range d.sess.MessagesSnapshot() {
		if item.Message == nil || item.Message.TurnID != msg.RequestID {
			continue
		}
		mode := msg.InputMode
		if mode == "" {
			mode = "turn"
		}

		storedMode := item.Message.InputMode
		if storedMode != "" && storedMode != mode {
			return false, false, &SessionError{Kind: SessionErrorConflict, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "submit"}
		}
		if normalizedInputOrigin(item.Message.InputOrigin) != normalizedInputOrigin(msg.InputOrigin) || item.Message.SenderID != msg.SenderID || item.Message.SenderName != msg.SenderName || item.Message.ReportOutcome != msg.ReportOutcome || item.Message.Message.Content != msg.Content || !reflect.DeepEqual(item.Message.Message.MultiContent, msg.MultiContent) || msg.Retry {
			return false, false, &SessionError{Kind: SessionErrorConflict, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "submit"}
		}
		return true, item.Message.Pending && d.queuedAtAdmissionLocked(msg), nil
	}
	return false, false, nil
}

func (d *sessionDriver) cancelForPersistence(err error) {
	var cancel context.CancelFunc
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		cancel = d.cancel

		d.lastError = err.Error()
		d.persistenceFailure = err
		d.publishPersistenceFailureLocked(err)
		return nil
	})
	if cancel != nil && !session.IsTemporary(err) {
		cancel()
	}
}

func (d *sessionDriver) publishPersistenceFailureLocked(err error) {
	key := "persistence:" + err.Error()
	if session.IsTemporary(err) {
		key = "persistence-retrying:" + d.activeRequestID
	}
	if d.lastFailureKey == key {
		return
	}
	d.lastFailureKey = key
	if session.IsTemporary(err) {
		d.events.PublishForRequest(d.sessionIDLocked(), d.activeRequestID, Warning("Session storage is retrying; accepted work is retained and durable completion is waiting: "+err.Error(), d.AgentNameLocked()))
		return
	}
	d.events.PublishForRequest(d.sessionIDLocked(), d.activeRequestID, ErrorForSession(d.sessionIDLocked(), "Session persistence failed; accepted work is retained and new turns are blocked until storage recovers: "+err.Error()))
}

// Legacy headless follow-ups alone remain dormant while idle; ordinary inputs
// may wake the mailbox and the run still starts from its FIFO head.
func (d *sessionDriver) pendingWakeableLocked() bool {
	if d.compactReserved || d.unidentifiedUncertain {
		return false
	}
	for _, msg := range d.pending {
		if msg.InputMode != "legacy_followup" {
			return true
		}
	}
	return false
}

func (d *sessionDriver) stopGenerationAfterTeamEdit(ctx context.Context, generation uint64) error {
	var cancel context.CancelFunc
	err := d.ownerCall(ctx, func() error {
		if d.generation != generation || d.stopped {
			return ErrSessionStopped
		}
		d.teamEditStopPending = true
		if d.cancel != nil {
			cancel = d.cancel
		}
		return nil
	})
	if cancel != nil {
		cancel()
	}
	return err
}

func (d *sessionDriver) consumeTeamEditStop(ctx context.Context) bool {
	stop := false
	_ = d.ownerCall(ctx, func() error {
		if d.teamEditStopPending {
			stop = true
			d.teamEditStopPending = false
		}
		return nil
	})
	return stop
}
