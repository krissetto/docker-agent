package runtime

import (
	"context"
	"hash/maphash"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
)

// sessionDriverRegistry owns the per-session drivers for a LocalRuntime.
type sessionDriverRegistry struct {
	r *LocalRuntime

	mu                 sync.Mutex
	publishing         [64]sync.Mutex
	publicationSeed    maphash.Seed
	drivers            map[string]*sessionDriver
	deleted            map[string]struct{}
	reservations       map[string]*restoreDriverReservation
	prepareRestoreHook func(string) // test-only barrier before reservation
	stoppedTrees       map[string]struct{}
	pendingClaims      map[string]int
	creating           map[string]chan struct{}
	closed             bool
	workOnce           sync.Once
	work               chan struct{}
	workDone           chan struct{}
	workStop           chan struct{}
	runMu              sync.Mutex
}

func newSessionDriverRegistry(r *LocalRuntime) *sessionDriverRegistry {
	return &sessionDriverRegistry{
		publicationSeed: maphash.MakeSeed(),
		r:               r,
		drivers:         map[string]*sessionDriver{},
		deleted:         map[string]struct{}{},
		reservations:    map[string]*restoreDriverReservation{},
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
		if g.ancestorResidentLocked(id) || g.pendingClaims[id] != 0 || g.reservations[id] != nil {
			continue
		}
		if now.Sub(d.registrySnapshot().lastActive) >= retention {
			g.reclaimDriverLocked(id, d)
		}
	}
}

func (g *sessionDriverRegistry) evictSettledForCapacityLocked() bool {
	ids := make([]string, 0, len(g.drivers))
	lastActive := make(map[string]time.Time, len(g.drivers))
	for id, d := range g.drivers {
		ids = append(ids, id)
		lastActive[id] = d.registrySnapshot().lastActive
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
		if g.ancestorResidentLocked(id) || g.pendingClaims[id] != 0 || g.reservations[id] != nil {
			continue
		}
		d := g.drivers[id]
		if d != nil && !d.registrySnapshot().active && !driverDrained(d) {
			return false
		}
		if d != nil && g.reclaimDriverLocked(id, d) {
			return true
		}
	}
	return false
}

func (g *sessionDriverRegistry) reclaimDriverLocked(id string, d *sessionDriver) bool {
	if !d.mu.TryLock() {
		return false
	}
	d.mu.Unlock()
	g.mu.Unlock()
	reclaimed := false
	_ = d.ownerCall(context.WithoutCancel(g.r.lifetime()), func() error {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.closed || g.drivers[id] != d || g.pendingClaims[id] != 0 || g.reservations[id] != nil || g.ancestorResidentLocked(id) || !d.beginReclaimLocked() {
			return nil
		}
		d.maintenanceRetired = true
		reclaimed = true
		return nil
	})
	if reclaimed {
		if err := d.retire(context.WithoutCancel(g.r.lifetime())); err != nil {
			reclaimed = false
		}
	}
	g.mu.Lock()
	if reclaimed && g.drivers[id] == d {
		delete(g.drivers, id)
		g.releasePersistence(id)
		if g.r != nil && g.r.sessionEvents != nil {
			g.r.sessionEvents.Delete(id)
		}
	}

	return reclaimed
}

func (g *sessionDriverRegistry) sessionBindingSnapshot(sess *session.Session) (*session.Session, string, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	clone := sess.OwnSnapshot()
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
	publicationLock := &g.publishing[maphash.String(g.publicationSeed, sess.ID)%uint64(len(g.publishing))]
	publicationLock.Lock()
	defer publicationLock.Unlock()
	parentPinned := g.pinResidentParent(sess.ParentID)
	if parentPinned {
		defer g.releaseUnpublishedClaim(sess.ParentID)
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
	var retired *sessionDriver
	replacing := 0
	if d != nil && d.registrySnapshot().stopped {
		if !d.registrySnapshot().settled || d.registrySnapshot().reclaiming {
			g.mu.Unlock()
			return nil, &SessionError{Kind: SessionErrorStopped, SessionID: sess.ID, Operation: "register"}
		}
		// Stop ends one session generation, not the durable session identity.
		// Once that generation has fully settled, an explicit CreateSession may
		// reopen the stable ID with a fresh session object. Stale handles retain
		// the old stopped driver. Delete is still final because deleted IDs are
		// rejected above before generation replacement.
		replacing = 1
		retired = d
		d = nil
	}
	if d == nil {
		maxSessions := g.maxSessionsLocked()
		if maxSessions > 0 && !limitAllows(len(g.drivers)-replacing, maxSessions) {
			g.evictSettledForCapacityLocked()
		}
		if !limitAllows(g.residentAndReservedLocked()-replacing, maxSessions) {
			g.mu.Unlock()
			return nil, &SessionError{Kind: SessionErrorCapacity, SessionID: sess.ID, Operation: SessionOperationCreateSession, Reason: SessionErrorReasonLimit, Limit: maxSessions}
		}
		if retired != nil {
			retired.maintenanceRetired = true
			g.mu.Unlock()
			if err := retired.retire(context.WithoutCancel(g.r.lifetime())); err != nil {
				return nil, err
			}
			g.mu.Lock()
			if g.closed || g.drivers[sess.ID] != retired {
				g.mu.Unlock()
				return nil, ErrSessionClosed
			}
		}
		// Preserve the stopped generation's observation state if admission fails.
		if replacing != 0 && g.r != nil {
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
		g.mu.Unlock()
		d.SetModelBinding(modelRef, providers)
		g.mu.Lock()
		if g.closed || g.drivers[sess.ID] != nil && replacing == 0 {
			g.mu.Unlock()
			d.closeOwner()
			return nil, &SessionError{Kind: SessionErrorClosed, SessionID: sess.ID, Operation: "register"}
		}
		g.drivers[sess.ID] = d
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
	boundAgent := sess.AgentName
	if bind {
		boundAgent = agentName
	}
	if d.identityID != sess.ID || d.registrySnapshot().agentName != boundAgent {
		g.mu.Unlock()
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: sess.ID, Operation: "replace_pinned_session"}
	}
	g.mu.Unlock()
	d.SetModelBinding(modelRef, providers)
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
	// Keep a resident parent routable while aggregate admission reclaims capacity.
	parentPinned := g.pinResidentParent(sess.ParentID)
	if parentPinned {
		defer g.releaseUnpublishedClaim(sess.ParentID)
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
	reserved := false
	defer func() {
		if !reserved {
			d.closeOwner()
		}
	}()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil, &SessionError{Kind: SessionErrorClosed, SessionID: sess.ID, Operation: "restore_prepare"}
	}
	if g.drivers[sess.ID] != replaces || g.reservations[sess.ID] != nil || (replaces != nil && (!replaces.registrySnapshot().replaceable || !driverDrained(replaces))) {
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: sess.ID, Operation: "restore_collision"}
	}
	if _, stopped := g.stoppedTrees[sess.ID]; stopped {
		return nil, &SessionError{Kind: SessionErrorStopped, SessionID: sess.ID, Operation: SessionOperationRestorePrepare}
	}
	if _, deleted := g.deleted[sess.ID]; deleted {
		return nil, &SessionError{Kind: SessionErrorStopped, SessionID: sess.ID, Operation: "restore_prepare"}
	}
	maxSessions := g.maxSessionsLocked()
	if replaces == nil {
		for maxSessions > 0 && len(g.reservations) < maxSessions && !limitAllows(g.residentAndReservedLocked(), maxSessions) {
			if !g.evictSettledForCapacityLocked() {
				break
			}
		}
		if !limitAllows(g.residentAndReservedLocked(), maxSessions) {
			return nil, &SessionError{Kind: SessionErrorCapacity, SessionID: sess.ID, Operation: SessionOperationRestorePrepare, Reason: SessionErrorReasonLimit, Limit: maxSessions}
		}
	}
	g.reservations[sess.ID] = reservation
	reserved = true
	return reservation, nil
}

func (r *restoreDriverReservation) Discard() {
	if r == nil || r.registry == nil {
		return
	}
	r.registry.mu.Lock()
	discarded := false
	defer func() {
		r.registry.mu.Unlock()
		if discarded {
			r.driver.closeOwner()
		}
	}()
	if r.registry.reservations[r.id] == r {
		delete(r.registry.reservations, r.id)
		discarded = true
		if r.registry.pendingClaims[r.id] == 0 && r.registry.drivers[r.id] == nil && r.registry.r != nil {
			r.registry.r.sessionService.release(r.registry.r, r.id)
		}
	}
}

// ActivateRestoreBatch atomically makes a prepared batch discoverable.
// Callers may hold manager.mu; registry code never acquires it.
func (g *sessionDriverRegistry) ActivateRestoreBatch(batch []*restoreDriverReservation, commit func() error) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return &SessionError{Kind: SessionErrorClosed, Operation: "restore_activate"}
	}
	for _, reservation := range batch {
		if reservation == nil || reservation.registry != g || g.reservations[reservation.id] != reservation || g.drivers[reservation.id] != reservation.replaces || reservation.replaces != nil && (!reservation.replaces.registrySnapshot().replaceable || !driverDrained(reservation.replaces)) {
			g.mu.Unlock()
			return &SessionError{Kind: SessionErrorInvalid, Operation: "restore_activate"}
		}
	}
	for _, reservation := range batch {
		if reservation.replaces != nil {
			reservation.replaces.maintenanceRetired = true
		}
	}
	g.mu.Unlock()
	for _, reservation := range batch {
		if reservation.replaces != nil {
			if err := reservation.replaces.drainRetirement(context.WithoutCancel(g.r.lifetime())); err != nil {
				return err
			}
		}
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return ErrSessionClosed
	}
	for _, reservation := range batch {
		if g.reservations[reservation.id] != reservation || g.drivers[reservation.id] != reservation.replaces {
			g.mu.Unlock()
			return ErrSessionStopped
		}
	}
	if commit != nil {
		if err := commit(); err != nil {
			g.mu.Unlock()
			return err
		}
	}
	for _, reservation := range batch {
		g.drivers[reservation.id] = reservation.driver
		delete(g.reservations, reservation.id)
	}
	g.mu.Unlock()
	for _, reservation := range batch {
		if reservation.replaces != nil {
			reservation.replaces.closeOwner()
		}
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
		current = d.identityParent
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

// PostReliable accepts a runtime-authored lifecycle note for a known session.
// Idle sessions retain the note even when run admission is temporarily denied.
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
	if !ok || g.closed {
		g.mu.Unlock()
		return false
	}
	g.mu.Unlock()
	return d.PostReliable(ctx, msg)
}

func (g *sessionDriverRegistry) PostOrBuffer(ctx context.Context, sessionID string, msg QueuedMessage, wake bool) bool {
	g.mu.Lock()
	d, ok := g.drivers[sessionID]
	if !ok || g.closed {
		g.mu.Unlock()
		return false
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
		return true
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
	d := g.drivers[sessionID]
	if d == nil || d != expected || !d.registrySnapshot().settled {
		g.mu.Unlock()
		return false
	}
	if g.pendingClaims == nil {
		g.pendingClaims = make(map[string]int)
	}
	g.pendingClaims[sessionID]++
	g.mu.Unlock()
	defer g.releaseUnpublishedClaim(sessionID)
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
	}
	g.mu.Unlock()
	if d != nil {
		if final {
			d.StopAllForDelete()
		} else {
			d.StopAll()
		}
	}
	if d != nil {
		d.refreshAttention()
		if err := d.retire(ctx); err != nil {
			return err
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
	defer g.mu.Unlock()
	if g.drivers[sessionID] == d {
		delete(g.drivers, sessionID)
		g.releasePersistence(sessionID)
	}
	delete(g.stoppedTrees, sessionID)
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
	g.mu.Unlock()
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
		if err := d.retire(ctx); err != nil {
			return err
		}
	}

	g.mu.Lock()
	for id := range g.drivers {
		g.releasePersistence(id)
	}
	for id := range g.reservations {
		drivers = append(drivers, g.reservations[id].driver)
		g.releasePersistence(id)
	}
	g.reservations = map[string]*restoreDriverReservation{}
	g.drivers = map[string]*sessionDriver{}
	g.mu.Unlock()
	for _, d := range drivers {
		d.closeOwner()
	}
	return nil
}

func (g *sessionDriverRegistry) Close() {
	_ = g.CloseContext(context.Background())
}

// Ancestors stay routable while any resident descendant can produce a report.
func (g *sessionDriverRegistry) residentAndReservedLocked() int {
	count := len(g.drivers)
	for id := range g.reservations {
		if g.drivers[id] == nil {
			count++
		}
	}
	return count
}

func (g *sessionDriverRegistry) ancestorResidentLocked(id string) bool {
	for _, reservation := range g.reservations {
		seen := make(map[string]bool)
		for parentID := reservation.driver.identityParent; parentID != "" && !seen[parentID]; {
			seen[parentID] = true
			if parentID == id {
				return true
			}
			parent := g.drivers[parentID]
			if parent == nil || parent.identityParent == parentID {
				break
			}
			parentID = parent.identityParent
		}
	}
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

func (g *sessionDriverRegistry) pinResidentParent(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if id == "" || g.drivers[id] == nil {
		return false
	}
	if g.pendingClaims == nil {
		g.pendingClaims = make(map[string]int)
	}
	g.pendingClaims[id]++
	return true
}

func (g *sessionDriverRegistry) beginClaim(id string) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return &SessionError{Kind: SessionErrorClosed, SessionID: id, Operation: SessionOperationCreateSession}
	}
	if g.pendingClaims == nil {
		g.pendingClaims = make(map[string]int)
	}
	g.pendingClaims[id]++
	g.mu.Unlock()
	if g.r != nil {
		if err := g.r.sessionService.claim(g.r, id); err != nil {
			g.releaseUnpublishedClaim(id)
			return err
		}
	}
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

// Reservations exist only for live creations; waiting callers retain no entries.
func (g *sessionDriverRegistry) reserveCreation(ctx context.Context, id string) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return nil, ErrSessionClosed
		}
		if done := g.creating[id]; done != nil {
			g.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		limit := g.maxSessionsLocked()
		if limit <= 0 {
			limit = maxSessionEventSubscribers
		}
		if len(g.creating) >= limit {
			g.mu.Unlock()
			return nil, &SessionError{Kind: SessionErrorCapacity, SessionID: id, Operation: SessionOperationCreateSession, Limit: limit}
		}
		if g.creating == nil {
			g.creating = make(map[string]chan struct{})
		}
		done := make(chan struct{})
		g.creating[id] = done
		g.mu.Unlock()
		return func() { g.mu.Lock(); defer g.mu.Unlock(); delete(g.creating, id); close(done) }, nil
	}
}
