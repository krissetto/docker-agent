package runtime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"time"
	"weak"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
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

	r  *LocalRuntime
	wg *driverWorkGroup

	mu   sync.Mutex
	sess *session.Session

	viewDormant bool

	completionErr      error
	persistenceFailure error
	reportRetry        *session.ChildReport
	completionRunErr   string
	completionInFlight bool

	startDone chan struct{}

	reclaiming bool

	retryRunning       bool
	turnChanged        chan struct{}
	completedTurns     []string
	recentRetries      map[string]bool
	pending            []QueuedMessage
	steering           []QueuedMessage
	interruptRequested bool
	steeringChanged    chan struct{}

	compactReserved    bool
	compactCancel      context.CancelFunc
	compactOperation   uint64
	switchReserved     bool
	skillOperationID   string
	skillCancel        context.CancelFunc
	skillGeneration    uint64
	modelRef           string
	modelProviders     []provider.Provider
	bindingVersion     uint64
	pauseCh            chan struct{}
	pauseGeneration    uint64
	pausePublished     uint64
	interactions       map[string]sessionInteraction
	events             *sessionEventHub
	lastError          string
	lastFailureKey     string
	lastActive         time.Time
	preStart           sessionStartGate
	beforePrepareStart func()
	abortStart         func()
	onStarted          map[int]func()
	onSettled          map[int]func()
	nextHookID         int
	rootActive         bool
	settled            chan struct{}
}

type sessionInteraction struct {
	kind       InteractionKind
	turnID     string
	generation uint64

	event Event
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

	if d.stopped {
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
		if d.compactReserved || d.starting() || d.skillOperationID != "" {
			err := busy(SessionOperationCompactBusy)
			err.Limit = d.pendingLimit()
			return err
		}
		if len(d.pending) != 0 || len(d.steering) != 0 {
			return &SessionError{Kind: SessionErrorCapacity, SessionID: sessionID, Operation: SessionOperationCompactPending, Reason: SessionErrorReasonPending, Limit: d.pendingLimit()}
		}
	case SessionOperationRunSkill:
		if d.skillOperationID != "" || d.running() || d.starting() || d.settling() || d.reclaiming || d.compactReserved || d.switchReserved || d.pauseCh != nil || len(d.pending) != 0 || len(d.steering) != 0 {
			return busy(op)
		}
	case SessionOperationSwitchAgent:
		if d.skillOperationID != "" || d.switchReserved || d.running() || d.starting() || d.settling() || d.reclaiming || d.compactReserved || d.pauseCh != nil || len(d.pending) != 0 || len(d.steering) != 0 || len(d.interactions) != 0 {
			return busy(op)
		}
	}
	return nil
}

func newSessionDriver(r *LocalRuntime, sess *session.Session) *sessionDriver {
	settled := make(chan struct{})
	close(settled)
	d := &sessionDriver{r: r, turnChanged: make(chan struct{}), wg: newDriverWorkGroup(), sess: sess, events: newSessionEventHubWithLimits(r.maxReplayEvents, r.maxReplayBytes), settled: settled, lastActive: time.Now(), interactions: map[string]sessionInteraction{}, onStarted: map[int]func(){}, onSettled: map[int]func(){}}
	if sess != nil {
		for position, item := range sess.MessagesSnapshot() {
			if item.Message == nil || !item.Message.Pending || !item.Message.Accepted {
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
	return d
}

func (d *sessionDriver) adopt(adopted []QueuedMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, msg := range adopted {
		if msg.RequestID != "" {
			if err := d.acceptInputLocked(&msg); err != nil {
				d.lastError = err.Error()
				continue
			}
		}
		d.pending = append(d.pending, msg)
	}
}

func (d *sessionDriver) Subscribe(buffer int) (seed []Event, events <-chan Event, cancel func()) {
	return d.events.Subscribe(d.sessionID(), buffer)
}

func (d *sessionDriver) Drive(ctx context.Context, sess *session.Session) <-chan Event {
	if sess != d.session() {
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

func (d *sessionDriver) acceptInputLocked(msg *QueuedMessage) error {
	if msg != nil && msg.Retry {
		if d.recentRetries == nil {
			d.recentRetries = map[string]bool{}
		}
		if len(d.recentRetries) >= defaultMaxSubagentMailbox {
			for id := range d.recentRetries {
				if id != d.activeRequestID {
					delete(d.recentRetries, id)
					break
				}
			}
		}
		d.recentRetries[msg.RequestID] = true
	}
	if msg == nil || msg.Retry || d.sess == nil {
		return nil
	}
	message := msg.sessionMessage()
	msg.InputMode = message.InputMode
	message.Pending = true
	message.Accepted = true
	message.TurnID = msg.RequestID
	if d.r.sessionStore != nil {
		msg.AcceptedPersisted = true
		if _, err := d.r.sessionStore.AddMessage(context.WithoutCancel(d.r.ctx()), d.sess.ID, message); err != nil {
			if errors.Is(err, session.ErrNotFound) {
				msg.AcceptedPersisted = false
			} else {
				return err
			}
		}
	}
	position := d.sess.AddMessageAt(message)
	msg.AcceptedPosition = position
	d.events.PublishForRequest(d.sess.ID, msg.RequestID, inputEventMetadata(PendingUserMessageAccepted(d.sess.ID, msg.RequestID, msg.Content, msg.MultiContent, position), *msg))
	return nil
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
	key := promotionFailureKey(turnID, err)
	if turnID == "" || d.lastFailureKey == key {
		return
	}
	d.lastFailureKey = key
	d.events.PublishForRequest(d.sessionIDLocked(), turnID, ErrorForSession(d.sessionIDLocked(), err.Error()))
}

func (d *sessionDriver) promoteInputLocked(msg QueuedMessage) error {
	if msg.Retry {
		return nil
	}
	if d.sess == nil {
		return &SessionError{Kind: SessionErrorInvalid, Operation: "promote_input"}
	}
	if msg.AcceptedPersisted {
		if err := d.r.sessionStore.PromotePendingUserMessage(context.WithoutCancel(d.r.ctx()), d.sess.ID, msg.RequestID); err != nil {
			return err
		}
	}
	if !d.sess.PromotePendingUserMessageByTurnID(msg.RequestID) {
		return &SessionError{Kind: SessionErrorStale, SessionID: d.sess.ID, RequestID: msg.RequestID, Operation: "promote_input"}
	}
	d.lastFailureKey = ""
	d.events.PublishForRequest(d.sess.ID, msg.RequestID, inputEventMetadata(PendingUserMessagePromoted(d.sess.ID, msg.RequestID, msg.Content, msg.MultiContent, msg.AcceptedPosition), msg))
	return nil
}

func (d *sessionDriver) PostSteer(ctx context.Context, msg QueuedMessage) bool {
	_, err := d.postSteer(ctx, msg)
	return err == nil
}

// postSteer returns queued when an accepted steer was demoted to the pending
// turn FIFO by an active compaction reservation or older pending input.
func (d *sessionDriver) postSteer(ctx context.Context, msg QueuedMessage) (queued bool, err error) {
	msg.InputMode = "steer"
	d.mu.Lock()
	defer d.mu.Unlock()
	if found, queued, err := d.existingInputLocked(msg); found || err != nil {
		return queued, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if admissionErr := d.admitLocked(SessionOperationSteer); admissionErr != nil {
		admissionErr.RequestID = msg.RequestID
		return false, admissionErr
	}
	if d.compactReserved || (len(d.pending) != 0 && (!d.running() || d.activeRequestID == "")) {
		if !limitAllows(len(d.pending), d.pendingLimit()) {
			return false, &SessionError{Kind: SessionErrorCapacity, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "steer", Reason: SessionErrorReasonLimit, Limit: d.pendingLimit()}
		}
		if err := d.acceptInputLocked(&msg); err != nil {
			return false, err
		}
		d.pending = append(d.pending, msg)
		return true, nil
	}
	if !limitAllows(len(d.steering), d.pendingLimit()) {
		return false, &SessionError{Kind: SessionErrorCapacity, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "steer", Reason: SessionErrorReasonLimit, Limit: d.pendingLimit()}
	}
	if err := d.acceptInputLocked(&msg); err != nil {
		return false, err
	}
	d.steering = append(d.steering, msg)
	d.refreshSteeringLocked()
	return false, nil
}

// DrainSteering promotes accepted guidance through the session-owned durable
// transition before making it visible to the provider loop. A failed durable
// promotion and every later FIFO item remain queued for a later boundary.
func (d *sessionDriver) DrainSteering() []QueuedMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	promoted := 0
	for promoted < len(d.steering) {
		if err := d.promoteInputLocked(d.steering[promoted]); err != nil {
			d.lastError = err.Error()
			break
		}
		promoted++
	}
	steering := append([]QueuedMessage(nil), d.steering[:promoted]...)
	d.steering = d.steering[promoted:]
	d.refreshSteeringLocked()
	if len(steering) != 0 {
		d.notifyTurnChangedLocked()
	}
	return steering
}

func (d *sessionDriver) schedulePendingRetry() {
	d.mu.Lock()
	if !d.stopped && !d.viewDormant && len(d.pending) != 0 {
		d.retryRunning = true
	}
	d.mu.Unlock()
	if d.r.sessionDrivers != nil {
		d.r.sessionDrivers.signalWork()
	}
}

func (d *sessionDriver) inputQueued(msg QueuedMessage) bool {
	if msg.RequestID == "" {
		return false
	}
	if msg.Retry {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, pending := range d.pending {
			if pending.RequestID == msg.RequestID {
				return true
			}
		}
		return false
	}
	if d.sess == nil {
		return false
	}
	for _, item := range d.sess.MessagesSnapshot() {
		if item.Message != nil && item.Message.TurnID == msg.RequestID {
			return item.Message.Pending
		}
	}
	return false
}

func (d *sessionDriver) Post(ctx context.Context, msg QueuedMessage, wake bool) bool {
	if msg.trustedSteering() {
		return d.postTrustedInput(ctx, msg)
	}
	_, err := d.post(ctx, msg, wake)
	return err == nil
}

// post returns queued when the accepted input remains in the pending FIFO at
// the admission commit point.
func (d *sessionDriver) post(ctx context.Context, msg QueuedMessage, wake bool) (queued bool, err error) {
	for {
		d.mu.Lock()
		if found, queued, err := d.existingInputLocked(msg); found || err != nil {
			d.mu.Unlock()
			return queued, err
		}
		// Non-waking posts historically classify an idle driver as stopped before
		// considering reservations; preserve that public ordering.
		if !d.stopped && !d.running() && !d.starting() && !wake {
			d.mu.Unlock()
			return false, &SessionError{Kind: SessionErrorStopped, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: SessionOperationPost}
		}
		if admissionErr := d.admitLocked(SessionOperationPost); admissionErr != nil {
			admissionErr.RequestID = msg.RequestID
			d.mu.Unlock()
			return false, admissionErr
		}
		if d.compactReserved {
			if !limitAllows(len(d.pending), d.pendingLimit()) {
				d.mu.Unlock()
				return false, &SessionError{Kind: SessionErrorCapacity, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "post", Reason: SessionErrorReasonLimit, Limit: d.pendingLimit()}
			}
			if msg.RequestID != "" {
				if err := d.acceptInputLocked(&msg); err != nil {
					d.mu.Unlock()
					return false, err
				}
			}
			d.pending = append(d.pending, msg)
			if normalizedInputOrigin(msg.InputOrigin) == session.InputOriginUser {
				d.authorizeViewLocked()
			}
			d.mu.Unlock()
			d.refreshAttention()
			return true, nil
		}
		if d.starting() {
			done := d.startDone
			d.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		if !limitAllows(len(d.pending), d.pendingLimit()) {
			d.mu.Unlock()
			return false, &SessionError{Kind: SessionErrorCapacity, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "post", Reason: SessionErrorReasonLimit, Limit: d.pendingLimit()}
		}
		if d.running() || d.settling() {
			if msg.RequestID != "" {
				if err := d.acceptInputLocked(&msg); err != nil {
					d.mu.Unlock()
					return false, err
				}
			}
			d.pending = append(d.pending, msg)
			if normalizedInputOrigin(msg.InputOrigin) == session.InputOriginUser {
				d.authorizeViewLocked()
			}
			d.mu.Unlock()
			d.refreshAttention()
			return true, nil
		}

		// A fresh command's durable admission is linearized under the same
		// lock as the running/starting/compaction checks above. Compaction cannot
		// reserve between classification and append.
		if msg.RequestID != "" {
			if err := d.acceptInputLocked(&msg); err != nil {
				d.mu.Unlock()
				return false, err
			}
		}
		d.pending = append(d.pending, msg)
		if normalizedInputOrigin(msg.InputOrigin) == session.InputOriginUser {
			d.authorizeViewLocked()
		}
		// Correlated non-retry input has crossed the durable Session/store commit
		// point. Retry and empty-ID lifecycle notes exist only in the mailbox.
		durable := msg.RequestID != "" && !msg.Retry
		beforePrepareStart := d.beforePrepareStart
		d.mu.Unlock()
		if beforePrepareStart != nil {
			beforePrepareStart()
		}
		wakeCtx, generation, callbacks, err := d.prepareStart(d.r.lifetime(), true)
		if err != nil {
			// Durable append already committed. Losing a wake to the session's
			// existing turn is success, not a promotion or mailbox-capacity failure.
			if startErr, ok := errors.AsType[*SessionError](err); ok && startErr.Operation == SessionOperationStart && startErr.Reason == SessionErrorReasonBusy {
				return d.inputQueued(msg), nil
			}
			d.mu.Lock()
			stopped := d.stopped

			affectedTurnID := ""
			if len(d.pending) != 0 {
				affectedTurnID = d.pending[0].RequestID
			}
			stillPending := false
			for _, pending := range d.pending {
				if pending.RequestID == msg.RequestID {
					stillPending = true
					break
				}
			}
			if stillPending && !stopped {
				d.lastError = err.Error()
				d.publishPromotionFailureLocked(affectedTurnID, err)
			}
			d.mu.Unlock()
			if stillPending && !stopped {
				d.schedulePendingRetry()
			}
			// Durable acceptance plus FIFO append is the public commit point.
			// A competing caller may already have promoted (and even completed)
			// this turn, while a wake failure leaves it pending for retry. Neither
			// state permits the caller to infer rejection and resubmit.
			if durable {
				return d.inputQueued(msg), nil
			}
			if stillPending {
				return true, nil
			}
			if !stopped {
				// A competing start already consumed this mailbox-only command.
				return false, nil
			}
			return false, err
		}
		for _, fn := range callbacks {
			fn()
		}
		queued := d.inputQueued(msg)
		d.startWake(wakeCtx, generation)
		return queued, nil
	}
}

// PostReliable appends a lifecycle note before attempting admission. Capacity
// denial is not delivery failure: the bounded pending mailbox retains the note
// for a later release signal. Stopped drivers and mailbox overflow still fail.
func (d *sessionDriver) PostReliable(ctx context.Context, msg QueuedMessage) bool {
	msg.InputOrigin = session.InputOriginRuntime
	msg.InputMode = "steer"
	return d.postTrustedInput(ctx, msg)
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
	d.mu.Lock()
	defer d.mu.Unlock()
	pending := d.pending
	d.pending = nil
	return pending
}

func (d *sessionDriver) DrainRuntimeNotes() []QueuedMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	notes := make([]QueuedMessage, 0, len(d.pending))
	pending := d.pending[:0]
	blocked := false
	for _, message := range d.pending {
		if !blocked && (message.RequestID == "" || message.trustedSteering()) {
			if message.RequestID != "" {
				if err := d.promoteInputLocked(message); err != nil {
					blocked = true
					pending = append(pending, message)
					continue
				}
			}
			notes = append(notes, message)
		} else {
			pending = append(pending, message)
		}
	}
	d.pending = pending
	d.refreshSteeringLocked()
	if len(notes) != 0 {
		d.notifyTurnChangedLocked()
	}
	return notes
}

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
	d.mu.Lock()
	defer d.mu.Unlock()
	d.preStart = fn
	d.abortStart = abort
}

func (d *sessionDriver) OnStarted(fn func()) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.onStarted == nil {
		d.onStarted = map[int]func(){}
	}
	d.nextHookID++
	id := d.nextHookID
	d.onStarted[id] = fn
	ref := weak.Make(d)
	return func() {
		d := ref.Value()
		if d == nil {
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.onStarted, id)
	}
}

func (d *sessionDriver) OnSettled(fn func()) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.onSettled == nil {
		d.onSettled = map[int]func(){}
	}
	d.nextHookID++
	id := d.nextHookID
	d.onSettled[id] = fn
	ref := weak.Make(d)
	return func() {
		d := ref.Value()
		if d == nil {
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.onSettled, id)
	}
}

// authorizeViewLocked is called only at successful explicit admission.
func (d *sessionDriver) authorizeViewLocked() bool {
	if !d.viewDormant {
		return false
	}
	d.viewDormant = false
	d.events.Publish(d.sessionIDLocked(), &DormancyChangedEvent{AgentContext: newAgentContext(d.AgentNameLocked()), Type: "session_dormancy_changed", SessionID: d.sessionIDLocked(), Dormant: false})
	return true
}

func (d *sessionDriver) TogglePause(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	d.mu.Lock()
	if admissionErr := d.admitLocked(SessionOperationPause); admissionErr != nil {
		d.mu.Unlock()
		return false, admissionErr
	}
	d.pauseGeneration++
	generation := d.pauseGeneration

	if d.pauseCh != nil {
		close(d.pauseCh)
		d.pauseCh = nil
		sessionID := d.sessionIDLocked()
		agentName := d.AgentNameLocked()
		d.mu.Unlock()
		d.events.Publish(sessionID, PauseChanged(sessionID, agentName, false))
		return false, nil
	}
	d.pauseCh = make(chan struct{})
	idle := d.skillOperationID == "" && !d.running() && !d.starting() && !d.settling() && len(d.pending) == 0
	sessionID := d.sessionIDLocked()
	agentName := d.AgentNameLocked()
	if idle {
		d.pausePublished = generation
	}
	d.mu.Unlock()
	d.events.Publish(sessionID, PauseChanged(sessionID, agentName, true))
	if idle {
		d.events.Publish(sessionID, Paused(sessionID, agentName))
	}
	return true, nil
}

func (d *sessionDriver) waitIfPaused(ctx context.Context, reached func()) (bool, error) {
	blocked := false
	for {
		d.mu.Lock()
		ch := d.pauseCh
		generation := d.pauseGeneration

		publish := ch != nil && d.pausePublished != generation
		if publish {
			d.pausePublished = generation
		}
		d.mu.Unlock()
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if admissionErr := d.admitLocked(SessionOperationSwitchAgent); admissionErr != nil {
		return nil, admissionErr
	}
	d.switchReserved = true
	defer func() { d.switchReserved = false }()
	if d.sess == nil || d.sess.ParentID != "" {
		return nil, &SessionError{Kind: SessionErrorUnsupported, SessionID: d.sessionIDLocked(), Operation: "switch_agent", Reason: SessionErrorReasonBusy}
	}
	return d.sess.Clone(), nil
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
	_, models := d.ModelSnapshot()
	return agent.WithContextModels(ctx, d.AgentName(), models)
}

func (d *sessionDriver) SetModelBinding(modelRef string, providers []provider.Provider) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.modelRef = modelRef
	d.modelProviders = slices.Clone(providers)
	d.bindingVersion++
}

func (d *sessionDriver) SetModelOverride(ctx context.Context, agentName, modelRef string, providers []provider.Provider) error {
	if d.r.sessionDrivers != nil {
		d.r.sessionDrivers.runMu.Lock()
		defer d.r.sessionDrivers.runMu.Unlock()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.sess == nil {
		return &SessionError{Kind: SessionErrorStopped, SessionID: d.sessionIDLocked(), Operation: "set_model"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	previous, previousCustom := d.sess.ModelStateSnapshot()
	d.sess.SetAgentModelOverride(agentName, modelRef)
	if d.r.sessionStore != nil {
		if err := d.r.sessionStore.UpdateSession(ctx, d.sess); err != nil {
			d.sess.ReplaceModelState(previous, previousCustom)
			return err
		}
	}
	d.modelRef = modelRef
	d.modelProviders = slices.Clone(providers)
	d.bindingVersion++
	return nil
}

func (d *sessionDriver) SetStarred(ctx context.Context, starred bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.sess == nil {
		return &SessionError{Kind: SessionErrorStopped, SessionID: d.sessionIDLocked(), Operation: "set_starred"}
	}
	previous := d.sess.Starred
	d.sess.Starred = starred
	if d.r.sessionStore != nil {
		if err := d.r.sessionStore.SetSessionStarred(ctx, d.sess.ID, starred); err != nil {
			d.sess.Starred = previous
			return err
		}
	}
	return nil
}

func (d *sessionDriver) RemoveAttachment(ctx context.Context, path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.sess == nil {
		return &SessionError{Kind: SessionErrorStopped, SessionID: d.sessionIDLocked(), Operation: "remove_attachment"}
	}
	if !d.sess.RemoveAttachedFile(path) {
		return &SessionError{Kind: SessionErrorNotFound, SessionID: d.sess.ID, Operation: "remove_attachment"}
	}
	if d.r.sessionStore != nil {
		if err := d.r.sessionStore.UpdateSession(ctx, d.sess); err != nil {
			d.sess.AddAttachedFile(path)
			return err
		}
	}
	return nil
}

func (d *sessionDriver) UpdateTitle(ctx context.Context, title string) error {
	d.mu.Lock()
	if d.stopped || d.sess == nil {
		sessionID := d.sessionIDLocked()
		d.mu.Unlock()
		return &SessionError{Kind: SessionErrorStopped, SessionID: sessionID, Operation: "update_title"}
	}
	previous := d.sess.TitleSnapshot()
	d.sess.SetTitle(title)
	if d.r.sessionStore != nil {
		if err := d.r.sessionStore.UpdateSessionTitle(ctx, d.sess.ID, title); err != nil {
			d.sess.SetTitle(previous)
			d.mu.Unlock()
			return err
		}
	}
	d.events.Publish(d.sess.ID, SessionTitle(d.sess.ID, title))
	d.mu.Unlock()
	return nil
}

type driverObservation struct {
	seed   []SequencedSessionEvent
	live   <-chan SequencedSessionEvent
	cancel func()

	cursor        uint64
	session       *session.Session
	status        SessionStatus
	interactions  []InteractionSnapshot
	pendingInputs []PendingInput
	position      int
}

func (d *sessionDriver) beginReclaimLocked() bool {
	if d.reclaiming || d.stopped || d.viewDormant || d.reportRetry != nil || d.running() || d.starting() || d.settling() || d.retryRunning || d.skillOperationID != "" || len(d.pending) != 0 || len(d.steering) != 0 || len(d.interactions) != 0 || d.events.HasSubscribers(d.sessionIDLocked()) {
		return false
	}
	d.reclaiming = true
	d.stopped = true
	d.skillGeneration++
	d.skillOperationID = ""
	d.skillCancel = nil
	return true
}

func (d *sessionDriver) observe(since *uint64, buffer int) driverObservation {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.reclaiming || d.stopped {
		return driverObservation{}
	}
	seed, live, cancel, cursor := d.events.SubscribeSequenced(d.sessionIDLocked(), since, buffer)
	cloned, status, interactions, pendingInputs, position := d.snapshotLocked()
	return driverObservation{seed: seed, live: live, cancel: cancel, cursor: cursor, session: cloned, status: status, interactions: interactions, pendingInputs: pendingInputs, position: position}
}

func (d *sessionDriver) Cancel(turnID string) CancelOutcome {
	d.mu.Lock()
	if !d.running() || d.cancel == nil || turnID == "" || d.activeRequestID != turnID {
		d.mu.Unlock()
		return CancelNotActive
	}
	if d.cancelling() {
		d.mu.Unlock()
		return CancelAlreadyCancelling
	}
	d.phase = sessionCancelling
	d.resolveInteractionsLocked()
	cancel := d.cancel

	d.mu.Unlock()
	cancel()
	return CancelAccepted
}

func (d *sessionDriver) StopAll() bool {
	return d.stopAll(false)
}

func (d *sessionDriver) StopAllForDelete() bool {
	return d.stopAll(true)
}

func (d *sessionDriver) stopAll(deleting bool) bool {
	d.mu.Lock()
	if deleting {
		d.events.FenceDelete(d.sessionIDLocked())
	}
	cancel := d.cancel

	compactCancel := d.compactCancel
	skillCancel := d.skillCancel
	pauseCh := d.pauseCh
	d.pauseCh = nil
	d.pending = nil
	d.stopped = true
	d.resolveInteractionsLocked()
	d.notifyTurnChangedLocked()
	d.skillGeneration++
	d.skillOperationID = ""
	d.skillCancel = nil
	d.signalStartDoneLocked()
	d.mu.Unlock()
	if pauseCh != nil {
		close(pauseCh)
	}
	if skillCancel != nil {
		skillCancel()
	}
	if compactCancel != nil {
		compactCancel()
	}
	if cancel == nil && compactCancel == nil {
		return false
	}
	if cancel != nil {
		cancel()
	}
	return true
}

func (d *sessionDriver) isStopped() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped
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
	}
	d.mu.Lock()
	d.interactions[requestID] = sessionInteraction{kind: kind, turnID: d.activeRequestID, generation: d.generation, event: payload}
	d.mu.Unlock()
	if d.r != nil && d.r.subagents != nil {
		d.refreshAttention()
	}
}

func (d *sessionDriver) Respond(response InteractionResponse) error {
	if response.InteractionID == "" {
		return &SessionError{Kind: SessionErrorInvalid, SessionID: d.sessionID(), Operation: "respond"}
	}
	d.mu.Lock()
	interaction, pending := d.interactions[response.InteractionID]
	if !pending {
		d.mu.Unlock()
		return &SessionError{Kind: SessionErrorStale, SessionID: d.sessionID(), RequestID: response.InteractionID, Operation: "respond"}
	}
	if interaction.turnID != d.activeRequestID || interaction.generation != d.generation {
		d.resolveInteractionLocked(response.InteractionID)
		d.mu.Unlock()
		d.refreshAttention()
		return &SessionError{Kind: SessionErrorStale, SessionID: d.sessionID(), RequestID: response.InteractionID, Operation: "respond_generation"}
	}
	d.mu.Unlock()
	if interaction.kind != response.Kind {
		return &SessionError{Kind: SessionErrorInvalid, SessionID: d.sessionID(), RequestID: response.InteractionID, Operation: "respond_kind"}
	}
	if d.r == nil || d.r.interactions == nil {
		return &SessionError{Kind: SessionErrorNotFound, SessionID: d.sessionID(), RequestID: response.InteractionID, Operation: "respond"}
	}
	switch response.Kind {
	case InteractionConfirmation, InteractionMaxIterations:
		if !IsValidResumeType(response.Resume.Type) {
			return &SessionError{Kind: SessionErrorInvalid, SessionID: d.sessionID(), RequestID: response.InteractionID, Operation: "respond_resume"}
		}
		response.Resume.Type = NormalizeResumeType(response.Resume.Type)
		response.Resume.SessionID = d.sessionID()
		response.Resume.RequestID = response.InteractionID
		if d.r.interactions.sendResume(d.sessionID(), response.Resume) {
			d.mu.Lock()
			d.resolveInteractionLocked(response.InteractionID)
			d.mu.Unlock()
			d.refreshAttention()
			return nil
		}
	case InteractionElicitation:
		if response.ElicitationID == "" {
			return &SessionError{Kind: SessionErrorInvalid, SessionID: d.sessionID(), RequestID: response.InteractionID, Operation: "respond_elicitation"}
		}
		if d.r.elicitationWaiters.resolve(response.ElicitationID, response.Elicitation) {
			d.mu.Lock()
			d.resolveInteractionLocked(response.InteractionID)
			d.mu.Unlock()
			d.refreshAttention()
			return nil
		}
	default:
		return &SessionError{Kind: SessionErrorInvalid, SessionID: d.sessionID(), RequestID: response.InteractionID, Operation: "respond"}
	}
	return &SessionError{Kind: SessionErrorStale, SessionID: d.sessionID(), RequestID: response.InteractionID, Operation: "respond"}
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
			pendingInputs = append(pendingInputs, PendingInput{TurnID: item.Message.TurnID, Content: item.Message.Message.Content, MultiContent: item.Message.Message.MultiContent, SessionPosition: index, InputOrigin: item.Message.InputOrigin, SenderID: item.Message.SenderID, SenderName: item.Message.SenderName, InputMode: item.Message.InputMode})
		}
	}
	interactions := make([]InteractionSnapshot, 0, len(d.interactions))
	for requestID, interaction := range d.interactions {
		item := InteractionSnapshot{SessionID: d.sessionIDLocked(), InteractionID: requestID, Kind: interaction.kind, Event: interaction.event}
		if elicitation, ok := interaction.event.(*ElicitationRequestEvent); ok {
			item.ElicitationID = elicitation.ElicitationID
		}
		interactions = append(interactions, item)
	}
	return cloned, d.statusLocked(), interactions, pendingInputs, position
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
		State: state, Pending: len(d.pending), TurnID: d.activeRequestID, LastError: d.lastError, Dormant: d.viewDormant,
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
// separate state and is never overtaken by compaction.
func (d *sessionDriver) compact(ctx context.Context, additionalPrompt string, sink EventSink) error {
	d.mu.Lock()
	sessionID := d.sessionIDLocked()
	if admissionErr := d.admitLocked(SessionOperationCompact); admissionErr != nil {
		d.mu.Unlock()
		return admissionErr
	}
	d.compactReserved = true
	d.compactOperation++
	operation := d.compactOperation
	if sink == nil {
		sink = EventSinkFunc(func(Event) {})
	}
	reservedSink := EventSinkFunc(func(event Event) {
		sink.Emit(event)
	})
	completeReservation := func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.compactOperation == operation {
			d.compactReserved = false
		}
	}
	if d.running() {
		d.mu.Unlock()
		err := d.r.compactLiveSession(ctx, sessionID, additionalPrompt, reservedSink, completeReservation)
		if err != nil {
			completeReservation()
		}
		return err
	}
	sess := d.sess
	if sess == nil {
		d.compactReserved = false
		d.mu.Unlock()
		return &SessionError{Kind: SessionErrorInvalid, Operation: "compact"}
	}
	if sink == nil {
		sink = EventSinkFunc(func(Event) {})
	}
	operationCtx, cancel := context.WithCancel(ctx)
	d.compactCancel = cancel
	d.phase = sessionStarting
	d.startDone = make(chan struct{})
	d.lastActive = time.Now()
	d.openSettledLocked()
	d.wg.Add(1)
	d.mu.Unlock()

	go func() {
		defer d.wg.Done()
		d.r.runLiveCompactionRequest(d.scopeModels(operationCtx), sess, liveCompactionRequest{additionalPrompt: additionalPrompt, events: reservedSink})

		d.mu.Lock()
		if d.compactOperation == operation {
			d.compactReserved = false
			d.compactCancel = nil
		}
		d.leave(sessionStarting)
		d.lastActive = time.Now()
		d.signalStartDoneLocked()
		d.closeSettledLocked()
		wakePending := !d.stopped && len(d.pending) != 0
		callbacks := d.settledCallbacksLocked()
		d.mu.Unlock()
		cancel()
		for _, fn := range callbacks {
			fn()
		}
		if wakePending {
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
	if d.r.sessionDrivers != nil {
		d.r.sessionDrivers.runMu.Lock()
		defer d.r.sessionDrivers.runMu.Unlock()
	}
	return d.prepareStartAdmissionLocked(ctx, wake)
}

func (d *sessionDriver) prepareStartAdmissionLocked(ctx context.Context, wake bool) (context.Context, uint64, []func(), error) {
	d.mu.Lock()
	dormant := d.viewDormant
	d.mu.Unlock()
	if dormant {
		return nil, 0, nil, &SessionError{Kind: SessionErrorInvalid, SessionID: d.sessionID(), Operation: "view_dormant"}
	}
	if d.r.sessionDrivers != nil {
		if err := d.r.sessionDrivers.admitRun(d); err != nil {
			return nil, 0, nil, err
		}
	}
	d.mu.Lock()
	if d.stopped || d.viewDormant || d.r.lifetime().Err() != nil {
		id := d.sessionIDLocked()
		d.mu.Unlock()
		return nil, 0, nil, &SessionError{Kind: SessionErrorStopped, SessionID: id, Operation: SessionOperationStart}
	}
	if d.running() || d.starting() || d.settling() {
		id := d.sessionIDLocked()
		d.mu.Unlock()
		return nil, 0, nil, &SessionError{Kind: SessionErrorCapacity, SessionID: id, Operation: SessionOperationStart, Reason: SessionErrorReasonBusy}
	}
	d.phase = sessionStarting
	d.startDone = make(chan struct{})
	gate := d.preStart
	abort := d.abortStart
	d.mu.Unlock()

	// The manager gate may take the manager mutex. Invoke it without d.mu so
	// manager stop/settle paths can never invert manager and driver locks.
	if gate != nil {
		if err := gate(); err != nil {
			d.mu.Lock()
			d.leave(sessionStarting)
			d.signalStartDoneLocked()
			d.mu.Unlock()
			return nil, 0, nil, err
		}
	}

	d.mu.Lock()
	if d.stopped || d.running() || !d.starting() {
		d.leave(sessionStarting)
		d.signalStartDoneLocked()
		d.mu.Unlock()
		if abort != nil {
			abort()
		}
		return nil, 0, nil, &SessionError{Kind: SessionErrorStopped, SessionID: d.sessionIDLocked(), Operation: "start"}
	}
	if wake && len(d.pending) == 0 {
		id := d.sessionIDLocked()
		d.leave(sessionStarting)
		d.signalStartDoneLocked()
		d.mu.Unlock()
		if abort != nil {
			abort()
		}
		return nil, 0, nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: SessionOperationWakePending}
	}
	if len(d.pending) > 0 {
		next := d.pending[0]
		if next.RequestID != "" {
			if err := d.promoteInputLocked(next); err != nil {
				d.lastError = err.Error()
				d.publishPromotionFailureLocked(next.RequestID, err)
				d.leave(sessionStarting)
				d.signalStartDoneLocked()
				d.mu.Unlock()
				if abort != nil {
					abort()
				}
				return nil, 0, nil, err
			}
			d.pending = d.pending[1:]
			d.activeRequestID = next.RequestID
		}
	}
	d.events.SetRequest(d.sess.ID, d.activeRequestID, d.generation+1)
	d.leave(sessionStarting)
	d.beginRun()
	d.generationResult = ""
	d.lastActive = time.Now()

	d.rootActive = d.sess != nil && !d.sess.IsSubSession()
	if d.rootActive {
		d.r.activeRootStreams.Add(1)
	}
	d.openSettledLocked()
	d.wg.Add(1)
	d.generation++
	generation := d.generation

	runCtx, cancel := context.WithCancel(ctx)
	stopLifetime := context.AfterFunc(d.r.lifetime(), cancel)
	d.cancel = func() { stopLifetime(); cancel() }
	callbacks := d.startedCallbacksLocked()
	d.signalStartDoneLocked()
	d.mu.Unlock()
	return runCtx, generation, callbacks, nil
}

func (d *sessionDriver) driveToOut(ctx context.Context, generation uint64, out chan Event) {
	defer d.wg.Done()
	defer close(out)
	d.driveToOutNoClose(ctx, generation, out)
}

func (d *sessionDriver) driveToOutNoClose(ctx context.Context, generation uint64, out chan Event) {
	for {
		nextCtx, nextGeneration, again := d.driveToOutGeneration(ctx, generation, out)
		if !again {
			return
		}
		ctx, generation = nextCtx, nextGeneration //nolint:fatcontext // successor context is independently rooted in supervisor lifetime
	}
}

func (d *sessionDriver) driveToOutGeneration(ctx context.Context, generation uint64, out chan Event) (context.Context, uint64, bool) {
	var runErr string
	cancelled := false
	ctx = d.scopeModels(ctx)
	run := d.r.runStreamRaw(ctx, d.session())
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
	ctx = d.scopeModels(ctx)
	for event := range d.r.runStreamRaw(ctx, d.session()) {
		if errEvent, ok := event.(*ErrorEvent); ok && runErr == "" {
			runErr = errEvent.Error
		}
		// Drained for flow control only; observers and the session event hub
		// publish the wake run to subscribers.
	}
	return d.finishRun(generation, runErr)
}

func (d *sessionDriver) finishRun(generation uint64, runErr string) (context.Context, uint64, bool) {
	return d.finishRunContext(d.r.durabilityContext(), generation, runErr)
}

func (d *sessionDriver) finishRunContext(ctx context.Context, generation uint64, runErr string) (context.Context, uint64, bool) {
	d.mu.Lock()
	if generation != d.generation || generation <= d.settledGeneration || d.completionInFlight {
		d.mu.Unlock()
		return nil, 0, false
	}
	if runErr == "" && !d.cancelling() && !d.stopped && !d.settling() && d.hasSteeringLocked() {
		nextCtx, cancel := context.WithCancel(d.r.lifetime())
		previousCancel := d.cancel
		d.cancel = cancel
		d.mu.Unlock()
		if previousCancel != nil {
			previousCancel()
		}
		return nextCtx, generation, true
	}
	d.lastError = runErr
	d.completionInFlight = true
	if !d.settling() {
		d.canceledOutcome = d.cancelling()
	}
	d.phase = sessionSettling
	turnID := d.activeRequestID
	d.mu.Unlock()
	var completionErr error
	for _, observer := range d.r.observers {
		if p, ok := observer.(*PersistenceObserver); ok {
			if err := p.completionErrorContext(ctx, d.sessionID()); err != nil {
				completionErr = err
				break
			}
		}
	}
	if completionErr == nil && d.r.subagents != nil {
		completionErr = d.r.subagents.completeSessionTurnContext(ctx, d, turnID, runErr)
	}
	if completionErr != nil {
		d.mu.Lock()
		d.completionInFlight = false
		firstFailure := d.completionErr == nil
		d.completionErr, d.completionRunErr = completionErr, runErr
		d.lastError = completionErr.Error()
		d.persistenceFailure = completionErr
		d.publishPersistenceFailureLocked(completionErr)
		d.notifyTurnChangedLocked()
		d.mu.Unlock()
		if firstFailure {
			d.r.sessionDrivers.signalWork()
		}
		return nil, 0, false
	}

	d.mu.Lock()
	d.leave(sessionSettling)
	d.completionInFlight = false
	d.settledGeneration = generation
	d.completionErr = nil
	d.persistenceFailure = nil
	d.lastFailureKey = ""
	d.resolveInteractionsLocked()
	// Publication shares the durable settlement guard and precedes waiter release
	// and successor promotion. The journal only queues events; it invokes no callbacks.
	if turnID != "" {
		outcome := TurnCompleted
		if d.canceledOutcome || d.stopped || d.r.lifetime().Err() != nil {
			outcome = TurnCanceled
		} else if runErr != "" {
			outcome = TurnFailed
		}
		d.events.PublishForRequest(d.sessionIDLocked(), turnID, &TurnSettledEvent{
			AgentContext: newAgentContext(d.AgentNameLocked()),
			Type:         "turn_settled", SessionID: d.sessionIDLocked(), TurnID: turnID, Outcome: outcome,
		})
	}
	d.completeTurnLocked(turnID)
	if len(d.steering) != 0 {
		d.pending = append(d.steering, d.pending...)
		d.steering = nil
		d.refreshSteeringLocked()
	}
	if !d.stopped && d.r.lifetime().Err() == nil && len(d.pending) > 0 {
		// Promote the oldest accepted request before the successor starts. This
		// is one atomic handoff: events from the next turn are correlated to the
		// request that caused it, and the old generation can no longer settle or
		// cancel the promoted generation.
		d.lastError = runErr
		d.generation++
		nextGeneration := d.generation
		next := d.pending[0]
		if d.sess != nil && next.RequestID != "" {
			if err := d.promoteInputLocked(next); err != nil {
				d.lastError = err.Error()
				d.publishPromotionFailureLocked(next.RequestID, err)
				failedRootActive := d.rootActive
				d.leave(sessionRunning)
				d.lastActive = time.Now()

				d.rootActive = false
				d.leave(sessionCancelling)
				d.cancel = nil
				d.activeRequestID = ""
				d.events.SetRequest(d.sess.ID, "", 0)
				d.closeSettledLocked()
				callbacks := d.settledCallbacksLocked()
				d.mu.Unlock()
				if failedRootActive {
					d.r.activeRootStreams.Add(-1)
				}
				for _, fn := range callbacks {
					fn()
				}
				if isRetryableSessionError(err) {
					d.schedulePendingRetry()
				}
				return nil, 0, false
			}
		}
		d.pending = d.pending[1:]
		d.activeRequestID = next.RequestID
		d.beginRun()
		d.generationResult = ""
		if d.sess != nil {
			d.events.SetRequest(d.sess.ID, d.activeRequestID, nextGeneration)
		}
		nextCtx, cancel := context.WithCancel(d.r.lifetime())
		d.cancel = cancel

		d.leave(sessionCancelling)
		d.mu.Unlock()
		return nextCtx, nextGeneration, true
	}
	pending := len(d.pending) > 0
	rootActive := d.rootActive
	var callbacks []func()
	d.lastError = runErr
	d.leave(sessionRunning)
	d.lastActive = time.Now()

	d.rootActive = false
	d.leave(sessionCancelling)
	d.cancel = nil
	d.activeRequestID = ""
	if d.sess != nil {
		d.events.SetRequest(d.sess.ID, "", 0)
	}
	d.closeSettledLocked()
	if !pending {
		callbacks = d.settledCallbacksLocked()
	}
	d.mu.Unlock()
	if rootActive {
		d.r.activeRootStreams.Add(-1)
	}
	for _, fn := range callbacks {
		fn()
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

func (d *sessionDriver) replaceSession(sess *session.Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sess = sess
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
	return d.sess
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
	for _, queued := range d.pending {
		if queued.RequestID != msg.RequestID {
			continue
		}
		if normalizedInputOrigin(queued.InputOrigin) != normalizedInputOrigin(msg.InputOrigin) || queued.SenderID != msg.SenderID || queued.SenderName != msg.SenderName || inputMode(queued.InputMode) != inputMode(msg.InputMode) || queued.Retry != msg.Retry || queued.Content != msg.Content || !reflect.DeepEqual(queued.MultiContent, msg.MultiContent) {
			return false, false, &SessionError{Kind: SessionErrorConflict, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "submit"}
		}
		return true, true, nil
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
		if normalizedInputOrigin(item.Message.InputOrigin) != normalizedInputOrigin(msg.InputOrigin) || item.Message.SenderID != msg.SenderID || item.Message.SenderName != msg.SenderName || item.Message.Message.Content != msg.Content || !reflect.DeepEqual(item.Message.Message.MultiContent, msg.MultiContent) || msg.Retry {
			return false, false, &SessionError{Kind: SessionErrorConflict, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: "submit"}
		}
		return true, item.Message.Pending, nil
	}
	return false, false, nil
}

func (d *sessionDriver) cancelForPersistence(err error) {
	d.mu.Lock()
	cancel := d.cancel

	d.lastError = err.Error()
	d.persistenceFailure = err
	d.publishPersistenceFailureLocked(err)
	d.mu.Unlock()
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
