package runtime

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
)

// sessionDriverRegistry owns the per-sessions for a LocalRuntime. A
// driver is created the first time the runtime sees a session object; detached
// notes for not-yet-seen sessions are kept as orphaned messages and adopted by
// the driver when the session appears.
type sessionDriverRegistry struct {
	r *LocalRuntime

	mu                 sync.Mutex
	drivers            map[string]*sessionDriver
	orphans            map[string][]QueuedMessage
	deleted            map[string]struct{}
	reservations       map[string]*restoreDriverReservation
	prepareRestoreHook func(string) // test-only barrier before reservation
	stoppedTrees       map[string]struct{}
	pendingClaims      map[string]int
	closed             bool
	workOnce           sync.Once
	work               chan struct{}
	workDone           chan struct{}
	workStop           chan struct{}
	runMu              sync.Mutex
}

func newSessionDriverRegistry(r *LocalRuntime) *sessionDriverRegistry {
	return &sessionDriverRegistry{
		r:            r,
		drivers:      map[string]*sessionDriver{},
		orphans:      map[string][]QueuedMessage{},
		deleted:      map[string]struct{}{},
		reservations: map[string]*restoreDriverReservation{},
	}
}

func (g *sessionDriverRegistry) GetInitialized(ctx context.Context, sess *session.Session) (*sessionDriver, error) {
	return g.getInitialized(ctx, sess)
}

func (g *sessionDriverRegistry) getInitialized(ctx context.Context, sess *session.Session) (*sessionDriver, error) {
	if g.r == nil || g.r.team == nil {
		return g.publishInitialized(sess, "", nil)
	}
	if sess == nil {
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "register"}
	}
	if sess.AgentName == "" {
		a, err := g.r.team.DefaultAgent()
		if err != nil {
			return nil, err
		}
		sess.AgentName = a.Name()
	}
	modelRef, providers, err := g.r.resolveSessionModelBinding(ctx, sess, "")
	if err != nil {
		return nil, err
	}
	return g.publishInitialized(sess, modelRef, providers)
}

// Get returns the driver for test and internal convenience callers. Production
// admission uses GetInitialized so it can propagate initialization errors.
// Get panics with that same error rather than silently returning a nil driver.
func (g *sessionDriverRegistry) Get(sess *session.Session) *sessionDriver {
	d, err := g.GetInitialized(context.Background(), sess)
	if err != nil {
		panic(err)
	}
	return d
}

func limitAllows(n, limit int) bool {
	return limit < 0 || n < limit
}

// maxSessionsLocked returns the configured session capacity.
func (g *sessionDriverRegistry) maxSessionsLocked() int {
	if g.r == nil {
		return 0
	}
	return g.r.maxSessions
}

func (g *sessionDriverRegistry) pruneIdleLocked(now time.Time) {
	if g.r == nil || g.r.idleRetention <= 0 {
		return
	}
	retention := g.r.idleRetention
	for id, d := range g.drivers {
		if g.ancestorResidentLocked(id) {
			continue
		}
		if !d.mu.TryLock() {
			continue
		}
		expired := now.Sub(d.lastActive) >= retention && d.beginReclaimLocked()
		d.mu.Unlock()
		if expired {
			delete(g.drivers, id)
			g.releasePersistence(id)
			if g.r.sessionEvents != nil {
				g.r.sessionEvents.Delete(id)
			}
		}
	}
}

func (g *sessionDriverRegistry) evictSettledForCapacityLocked() bool {
	ids := make([]string, 0, len(g.drivers))
	lastActive := make(map[string]time.Time, len(g.drivers))
	busyTime := time.Now()
	for id, d := range g.drivers {
		ids = append(ids, id)
		lastActive[id] = busyTime
		if d.mu.TryLock() {
			lastActive[id] = d.lastActive
			d.mu.Unlock()
		}
	}
	// The comparator observes one immutable snapshot, including one shared
	// fallback timestamp for busy drivers; it never waits on session I/O.
	slices.SortFunc(ids, func(a, b string) int {
		if cmp := lastActive[a].Compare(lastActive[b]); cmp != 0 {
			return cmp
		}
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
		return 0
	})
	for _, id := range ids {
		if g.ancestorResidentLocked(id) {
			continue
		}
		d := g.drivers[id]
		if !d.mu.TryLock() {
			continue
		}
		eligible := d.beginReclaimLocked()
		d.mu.Unlock()
		if !eligible {
			continue
		}
		delete(g.drivers, id)
		g.releasePersistence(id)
		if g.r.sessionEvents != nil {
			g.r.sessionEvents.Delete(id)
		}
		return true
	}
	return false
}

func (g *sessionDriverRegistry) sessionBindingSnapshot(sess *session.Session) (*session.Session, string, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	clone := sess.Clone()
	return clone, clone.AgentName, clone.MaxIterations
}

func (g *sessionDriverRegistry) publishInitialized(sess *session.Session, modelRef string, providers []provider.Provider) (*sessionDriver, error) {
	return g.publishInitializedWithBinding(sess, modelRef, providers, "", 0, false, false)
}

// publishInitializedWithBinding applies CreateSession's computed binding only
// when this call publishes a new driver. Registry locking keeps concurrent
// callers from observing or mutating a partially bound shared session.
func (g *sessionDriverRegistry) publishInitializedWithBinding(sess *session.Session, modelRef string, providers []provider.Provider, agentName string, maxIterations int, stampAgent, bind bool) (*sessionDriver, error) {
	if sess == nil {
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "register"}
	}
	if err := g.beginClaim(sess.ID); err != nil {
		return nil, err
	}
	defer g.releaseUnpublishedClaim(sess.ID)
	if !bind && sess.AgentName == "" && g.r != nil && g.r.team != nil {
		defaultAgent, err := g.r.team.DefaultAgent()
		if err != nil {
			return nil, err
		}
		sess.AgentName = defaultAgent.Name()
	}
	g.mu.Lock()
	if g.reservations[sess.ID] != nil {
		g.mu.Unlock()
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: sess.ID, Operation: "restore_reserved"}
	}
	g.pruneIdleLocked(time.Now())
	if g.closed {
		g.mu.Unlock()
		return nil, &SessionError{Kind: SessionErrorClosed, SessionID: sess.ID, Operation: "register"}
	}
	if _, stopped := g.stoppedTrees[sess.ID]; stopped {
		g.mu.Unlock()
		return nil, &SessionError{Kind: SessionErrorStopped, SessionID: sess.ID, Operation: SessionOperationCreateSession}
	}
	if _, deleted := g.deleted[sess.ID]; deleted {
		g.mu.Unlock()
		return nil, &SessionError{Kind: SessionErrorStopped, SessionID: sess.ID, Operation: "register"}
	}
	d := g.drivers[sess.ID]
	replacing := 0
	adopted := g.orphans[sess.ID]
	if d != nil && d.isStopped() {
		if !d.stoppedAndSettled() {
			g.mu.Unlock()
			return nil, &SessionError{Kind: SessionErrorStopped, SessionID: sess.ID, Operation: "register"}
		}
		// Stop ends one session generation, not the durable session identity.
		// Once that generation has fully settled, an explicit CreateSession may
		// reopen the stable ID with a fresh session object. Stale handles retain
		// the old stopped driver. Delete is still final because deleted IDs are
		// rejected above before generation replacement.
		replacing = 1
		d = nil
	}
	if d == nil {
		if g.r != nil {
			if err := g.r.sessionService.claim(g.r, sess.ID); err != nil {
				g.mu.Unlock()
				return nil, err
			}
		}
		maxSessions := g.maxSessionsLocked()
		if maxSessions > 0 && !limitAllows(len(g.drivers)-replacing, maxSessions) {
			g.evictSettledForCapacityLocked()
		}
		if !limitAllows(len(g.drivers)+len(g.reservations)-replacing, maxSessions) {
			g.mu.Unlock()
			return nil, &SessionError{Kind: SessionErrorCapacity, SessionID: sess.ID, Operation: SessionOperationCreateSession, Reason: SessionErrorReasonLimit, Limit: maxSessions}
		}
		// Preserve the stopped generation's observation state if admission fails.
		if replacing != 0 && g.r != nil {
			if g.r.interactions != nil {
				g.r.interactions.deleteSession(sess.ID)
			}
			if g.r.sessionEvents != nil {
				g.r.sessionEvents.Delete(sess.ID)
			}
		}
		if bind {
			sess.AgentName = agentName
			sess.MaxIterations = maxIterations
			if stampAgent {
				sess.SetAttribute(SessionAgentAttribute, agentName)
			}
		}
		d = newSessionDriver(g.r, sess)
		d.SetModelBinding(modelRef, providers)
		d.adopt(adopted)
		g.drivers[sess.ID] = d
		delete(g.orphans, sess.ID)
		g.mu.Unlock()
		if g.r != nil && g.r.agents != nil && g.r.subagents != nil && d.identityParent == "" {
			g.r.subagents.bindPublishedRootDriver(d)
		}
		// Accepted pending inputs are durable work, not a UI wake hint. A fresh
		// fully initialized runtime session resumes them automatically after
		// publication; small test/service stubs may not have an execution router.
		if g.r != nil && g.r.agents != nil {
			d.WakePending()
			g.signalWork()
		}
		return d, nil
	}
	current := d.session()
	boundAgent := sess.AgentName
	if bind {
		boundAgent = agentName
	}
	if current != sess || (current != nil && current.AgentName != boundAgent) {
		g.mu.Unlock()
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: sess.ID, Operation: "replace_pinned_session"}
	}
	d.SetModelBinding(modelRef, providers)
	d.adopt(adopted)
	delete(g.orphans, sess.ID)
	g.mu.Unlock()
	if g.r != nil && g.r.agents != nil && g.r.subagents != nil && d.identityParent == "" {
		g.r.subagents.bindPublishedRootDriver(d)
	}
	return d, nil
}

type restoreDriverReservation struct {
	registry *sessionDriverRegistry
	driver   *sessionDriver
	id       string
	replaces *sessionDriver
}

// PrepareRestore constructs a driver and resolves its model binding while it is
// detached from global lookup, orphan adoption, pruning, and execution.
func (g *sessionDriverRegistry) PrepareRestore(ctx context.Context, sess *session.Session) (*restoreDriverReservation, error) {
	return g.prepareRestore(ctx, sess, nil)
}

func (g *sessionDriverRegistry) prepareRestore(ctx context.Context, sess *session.Session, replaces *sessionDriver) (*restoreDriverReservation, error) {
	if sess == nil || sess.ID == "" {
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "restore_prepare"}
	}
	if err := g.beginClaim(sess.ID); err != nil {
		return nil, err
	}
	defer g.releaseUnpublishedClaim(sess.ID)
	if g.prepareRestoreHook != nil {
		g.prepareRestoreHook(sess.ID)
	}
	var modelRef string
	var providers []provider.Provider
	if g.r.team != nil {
		if sess.AgentName == "" {
			a, err := g.r.team.DefaultAgent()
			if err != nil {
				return nil, err
			}
			sess.AgentName = a.Name()
		}
		var err error
		modelRef, providers, err = g.r.resolveSessionModelBinding(ctx, sess, "")
		if err != nil {
			return nil, err
		}
	}
	d := newSessionDriver(g.r, sess)
	d.SetModelBinding(modelRef, providers)
	reservation := &restoreDriverReservation{registry: g, driver: d, id: sess.ID, replaces: replaces}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, &SessionError{Kind: SessionErrorClosed, SessionID: sess.ID, Operation: "restore_prepare"}
	}
	if g.drivers[sess.ID] != replaces || g.reservations[sess.ID] != nil || (replaces != nil && !replaces.stoppedViewReplaceable()) {
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: sess.ID, Operation: "restore_collision"}
	}
	if _, stopped := g.stoppedTrees[sess.ID]; stopped {
		return nil, &SessionError{Kind: SessionErrorStopped, SessionID: sess.ID, Operation: SessionOperationRestorePrepare}
	}
	if _, deleted := g.deleted[sess.ID]; deleted {
		return nil, &SessionError{Kind: SessionErrorStopped, SessionID: sess.ID, Operation: "restore_prepare"}
	}
	g.reservations[sess.ID] = reservation
	return reservation, nil
}

func (r *restoreDriverReservation) Discard() {
	if r == nil || r.registry == nil {
		return
	}
	r.registry.mu.Lock()
	defer r.registry.mu.Unlock()
	if r.registry.reservations[r.id] == r {
		delete(r.registry.reservations, r.id)
		if r.registry.pendingClaims[r.id] == 0 && r.registry.drivers[r.id] == nil && r.registry.r != nil {
			r.registry.r.sessionService.release(r.registry.r, r.id)
		}
	}
}

// ActivateRestoreBatch atomically makes a prepared batch discoverable and only
// then adopts unchanged orphan messages. Lock order: restoreMu -> manager.mu ->
// registry.mu; registry code never acquires manager.mu.
func (g *sessionDriverRegistry) ActivateRestoreBatch(batch []*restoreDriverReservation, commit func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return &SessionError{Kind: SessionErrorClosed, Operation: "restore_activate"}
	}
	maxSessions := g.maxSessionsLocked()
	for len(batch) == 1 && maxSessions > 0 && len(g.drivers)+len(batch) > maxSessions {
		if !g.evictSettledForCapacityLocked() {
			break
		}
	}
	if maxSessions == 0 || (maxSessions > 0 && len(g.drivers)+len(batch) > maxSessions) {
		return &SessionError{Kind: SessionErrorCapacity, Operation: "restore_activate", Reason: SessionErrorReasonLimit, Limit: maxSessions}
	}
	for _, reservation := range batch {
		if reservation == nil || reservation.registry != g || g.reservations[reservation.id] != reservation || g.drivers[reservation.id] != nil {
			return &SessionError{Kind: SessionErrorInvalid, Operation: "restore_activate"}
		}
	}
	if commit != nil {
		if err := commit(); err != nil {
			return err
		}
	}
	for _, reservation := range batch {
		reservation.driver.adopt(g.orphans[reservation.id])
		g.drivers[reservation.id] = reservation.driver
		delete(g.orphans, reservation.id)
		delete(g.reservations, reservation.id)
	}
	return nil
}

func (g *sessionDriverRegistry) Lookup(sessionID string) (*sessionDriver, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.drivers[sessionID]
	return d, ok
}

func (g *sessionDriverRegistry) ElicitationRoute(sessionID string) []string {
	g.mu.Lock()
	drivers := make(map[string]*sessionDriver, len(g.drivers))
	maps.Copy(drivers, g.drivers)
	g.mu.Unlock()

	route := make([]string, 0, len(drivers)+1)
	seen := make(map[string]struct{}, len(drivers)+1)
	for current := sessionID; current != "" && len(route) <= len(drivers); {
		if _, duplicate := seen[current]; duplicate {
			break
		}
		seen[current] = struct{}{}
		route = append(route, current)
		d := drivers[current]
		if d == nil {
			break
		}
		sess := d.session()
		if sess == nil {
			break
		}
		current = sess.ParentID
	}
	return route
}

func (g *sessionDriverRegistry) PostKnown(ctx context.Context, sessionID string, msg QueuedMessage, wake bool) bool {
	d, ok := g.Lookup(sessionID)
	if !ok {
		return false
	}
	return d.Post(ctx, msg, wake)
}

// PostReliable accepts a runtime-authored lifecycle note for eventual
// processing. Known idle sessions retain the note even when run admission is
// temporarily denied; unknown sessions use the same bounded orphan mailbox as
// other buffered delivery.
func (g *sessionDriverRegistry) PostReliable(ctx context.Context, sessionID string, msg QueuedMessage) bool {
	msg.InputOrigin = session.InputOriginRuntime
	msg.InputMode = "steer"
	if msg.RequestID == "" {
		id, err := newSessionRequestID()
		if err != nil {
			return false
		}
		msg.RequestID = id
	}
	g.mu.Lock()
	d, ok := g.drivers[sessionID]
	if !ok {
		if g.closed || !limitAllows(len(g.orphans[sessionID]), g.orphanLimitLocked()) {
			g.mu.Unlock()
			return false
		}
		g.orphans[sessionID] = append(g.orphans[sessionID], msg)
		g.mu.Unlock()
		return true
	}
	g.mu.Unlock()
	return d.PostReliable(ctx, msg)
}

func (g *sessionDriverRegistry) orphanLimitLocked() int {
	if g.r != nil {
		return g.r.maxOrphanMailbox
	}
	return 0
}

func (g *sessionDriverRegistry) PostOrBuffer(ctx context.Context, sessionID string, msg QueuedMessage, wake bool) bool {
	g.mu.Lock()
	d, ok := g.drivers[sessionID]
	if !ok {
		if g.closed || !wake || !limitAllows(len(g.orphans[sessionID]), g.orphanLimitLocked()) {
			g.mu.Unlock()
			return false
		}
		g.orphans[sessionID] = append(g.orphans[sessionID], msg)
		g.mu.Unlock()
		return true
	}
	g.mu.Unlock()
	return d.Post(ctx, msg, wake)
}

func (g *sessionDriverRegistry) Drain(sessionID string) []QueuedMessage {
	d, ok := g.Lookup(sessionID)
	if !ok {
		return nil
	}
	return d.DrainPending()
}

func (g *sessionDriverRegistry) HasPending(sessionID string) bool {
	d, ok := g.Lookup(sessionID)
	return ok && d.HasPending()
}

func (g *sessionDriverRegistry) StopAll(sessionID string) bool {
	d, ok := g.Lookup(sessionID)
	return ok && d.StopAll()
}

func (g *sessionDriverRegistry) Settled(sessionID string) bool {
	d, ok := g.Lookup(sessionID)
	if !ok {
		g.mu.Lock()
		defer g.mu.Unlock()
		return len(g.orphans[sessionID]) == 0
	}
	return d.Settled()
}

func (g *sessionDriverRegistry) Delete(ctx context.Context, sessionID string) error {
	return g.remove(ctx, sessionID, nil, true)
}

// Release removes one session instance without tombstoning its stable ID. It is
// for ephemeral protocol views and durable reload handoff; explicit Delete is
// final for the lifetime of this registry.
func (g *sessionDriverRegistry) Release(ctx context.Context, sessionID string) error {
	return g.remove(ctx, sessionID, nil, false)
}

// ReleaseDriver removes only the generation represented by expected. A stale
// view cleanup cannot tear down a newer generation registered under the same
// stable session ID.
func (g *sessionDriverRegistry) ReplaceSettledSession(sessionID string, expected *sessionDriver, sess *session.Session) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.drivers[sessionID]
	if d == nil || d != expected || !d.Settled() {
		return false
	}
	d.replaceSession(sess)
	return true
}

func (g *sessionDriverRegistry) ReleaseDriver(ctx context.Context, sessionID string, expected *sessionDriver) error {
	return g.remove(ctx, sessionID, expected, false)
}

func (g *sessionDriverRegistry) remove(ctx context.Context, sessionID string, expected *sessionDriver, final bool) error {
	g.mu.Lock()
	d := g.drivers[sessionID]
	if expected != nil && d != expected {
		g.mu.Unlock()
		return nil
	}
	if d != nil {
		d.maintenanceRetired = true
	}
	if final {
		g.deleted[sessionID] = struct{}{}
		if d != nil {
			// Fence admission while the registry still owns this generation, then
			// capture Done only after stopped prevents any later work-group Add.
			d.StopAllForDelete()
		}
	} else if d != nil {
		d.StopAll()
	}
	var done <-chan struct{}
	if d != nil {
		done = d.Done()
	}
	g.mu.Unlock()
	if d != nil {
		d.refreshAttention()
	}

	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// A drained goroutine can still own an uncommitted journal/outcome. Never
	// discard that authority on release/delete; retry the common barrier first.
	if d != nil {
		d.mu.Lock()
		settling, generation, runErr := d.settling(), d.generation, d.completionRunErr
		d.mu.Unlock()
		if settling {
			d.finishRunContext(ctx, generation, runErr)
			d.mu.Lock()
			completionErr := d.completionErr
			d.mu.Unlock()
			if completionErr != nil {
				return completionErr
			}
		}
	}

	if final && d != nil {
		sess := d.session()
		agentName := ""
		if sess != nil {
			agentName = sess.AgentName
		}
		d.events.TerminalAndDelete(sessionID, agentName)
	}

	g.mu.Lock()
	if g.drivers[sessionID] == d {
		delete(g.drivers, sessionID)
		g.releasePersistence(sessionID)
	}
	delete(g.orphans, sessionID)
	delete(g.stoppedTrees, sessionID)
	g.mu.Unlock()
	if g.r != nil && g.r.interactions != nil {
		g.r.interactions.deleteSession(sessionID)
	}
	return nil
}

// stopSchedulerLocked fences worker reservations before any drain snapshots.
func (g *sessionDriverRegistry) stopSchedulerLocked() {
	if g.workStop != nil {
		select {
		case <-g.workStop:
		default:
			close(g.workStop)
		}
	}
}

func (g *sessionDriverRegistry) closeAdmission() {
	g.mu.Lock()
	g.closed = true
	g.stopSchedulerLocked()
	drivers := make([]*sessionDriver, 0, len(g.drivers))
	for _, d := range g.drivers {
		drivers = append(drivers, d)
	}
	g.mu.Unlock()
	for _, d := range drivers {
		d.StopAll()
		d.refreshAttention()
	}
	g.signalWork()
}

func (g *sessionDriverRegistry) CloseContext(ctx context.Context) error {
	g.mu.Lock()
	g.closed = true
	g.stopSchedulerLocked()
	drivers := make([]*sessionDriver, 0, len(g.drivers))
	for _, d := range g.drivers {
		drivers = append(drivers, d)
	}
	workDone := g.workDone
	g.mu.Unlock()
	for _, d := range drivers {
		d.StopAll()
	}
	if workDone != nil {
		select {
		case <-workDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	for _, d := range drivers {
		select {
		case <-d.Done():
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Execution has stopped, but a failed journal or child commit still owns
	// its driver. Reuse the settlement barrier under this drain's context;
	// a later CloseContext can retry without executing accepted work again.
	for _, d := range drivers {
		d.mu.Lock()
		settling := d.settling()
		generation, runErr := d.generation, d.completionRunErr
		d.mu.Unlock()
		if settling {
			d.finishRunContext(ctx, generation, runErr)
			d.mu.Lock()
			err := d.completionErr
			d.mu.Unlock()
			if err != nil {
				return err
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.drivers {
		g.releasePersistence(id)
	}
	for id := range g.reservations {
		g.releasePersistence(id)
	}
	g.reservations = map[string]*restoreDriverReservation{}
	g.drivers = map[string]*sessionDriver{}
	g.orphans = map[string][]QueuedMessage{}
	return nil
}

func (g *sessionDriverRegistry) Close() {
	_ = g.CloseContext(context.Background())
}

// Ancestors stay routable while any resident descendant can produce a report.
func (g *sessionDriverRegistry) ancestorResidentLocked(id string) bool {
	for otherID, d := range g.drivers {
		if otherID == id {
			continue
		}
		parentID := d.identityParent
		seen := map[string]bool{}
		for parentID != "" && !seen[parentID] {
			if parentID == id {
				return true
			}
			seen[parentID] = true
			parent := g.drivers[parentID]
			if parent == nil {
				break
			}
			parentID = parent.identityParent
		}
	}
	return false
}

func (g *sessionDriverRegistry) beginClaim(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return &SessionError{Kind: SessionErrorClosed, SessionID: id, Operation: SessionOperationCreateSession}
	}
	if g.r != nil {
		if err := g.r.sessionService.claim(g.r, id); err != nil {
			return err
		}
	}
	if g.pendingClaims == nil {
		g.pendingClaims = make(map[string]int)
	}
	g.pendingClaims[id]++
	return nil
}

func (g *sessionDriverRegistry) releaseUnpublishedClaim(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pendingClaims[id]--
	if g.pendingClaims[id] <= 0 {
		delete(g.pendingClaims, id)
	}
	if g.pendingClaims[id] == 0 && g.drivers[id] == nil && g.reservations[id] == nil && g.r != nil {
		g.r.sessionService.release(g.r, id)
	}
}

func (g *sessionDriverRegistry) releasePersistence(id string) {
	if g.r == nil {
		return
	}
	if g.pendingClaims[id] == 0 {
		g.r.sessionService.release(g.r, id)
	}
	for _, observer := range g.r.observers {
		if p, ok := observer.(*PersistenceObserver); ok {
			p.release(id)
		}
	}
}
