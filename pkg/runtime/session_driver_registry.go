package runtime

import (
	"context"
	"errors"
	"hash/maphash"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// sessionDriverRegistry owns the per-session drivers for a LocalRuntime.
type sessionDriverRegistry struct {
	r *LocalRuntime

	mu                 sync.Mutex
	publishing         [64]sync.Mutex
	initializing       [64]sync.Mutex
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
	if sess == nil {
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "register"}
	}
	// Initialization for one session ID is serialized, so a first publication's
	// writes to the caller's pointer never overlap another initializer's reads or
	// writes. A resident driver owns its session: those callers initialize a
	// private snapshot and never write the shared pointer.
	initLock := &g.initializing[maphash.String(g.publicationSeed, sess.ID)%uint64(len(g.initializing))]
	initLock.Lock()
	defer initLock.Unlock()
	_, resident := g.Lookup(sess.ID)
	found := false
	if !resident {
		var err error
		if found, err = g.adoptStoredIdentity(ctx, sess); err != nil {
			return nil, err
		}
	}
	if g.r == nil || g.r.team == nil && g.r.teamEnvironments == nil {
		if !resident && !found {
			if _, err := g.persistFirstTime(ctx, sess, nil); err != nil {
				return nil, err
			}
		}
		return g.publishInitialized(sess, "", nil)
	}
	if resident {
		sess = sess.OwnSnapshot()
	}
	tm := g.r.team
	var initialLease *TeamEnvironmentLease
	if g.r.teamEnvironments != nil {
		var leaseErr error
		if env := teamEnvironmentFromContext(ctx); env != nil {
			initialLease, leaseErr = g.r.teamEnvironments.Acquire(env.RevisionID())
		} else {
			initialLease, leaseErr = g.r.teamEnvironments.AcquireCurrent()
		}
		if leaseErr != nil {
			return nil, leaseErr
		}
		tm = initialLease.Environment().Team()
	}
	if initialLease != nil {
		defer func() {
			if initialLease != nil {
				_ = initialLease.Release(context.WithoutCancel(g.r.lifetime()))
			}
		}()
	}
	if err := resolveInitialAgent(tm, sess); err != nil {
		return nil, err
	}
	if !resident && !found {
		adopted, err := g.persistFirstTime(ctx, sess, tm)
		if err != nil {
			return nil, err
		}
		if adopted {
			if err := resolveInitialAgent(tm, sess); err != nil {
				return nil, err
			}
		}
	}
	modelRef, providers, err := g.r.resolveSessionModelBinding(environmentScopedContext(ctx, initialLease), sess, "")
	if err != nil {
		return nil, err
	}
	driver, err := g.publishInitializedWithBindingAndEnvironment(ctx, sess, modelRef, providers, sess.AgentName, 0, false, false, initialLease)
	if err == nil {
		initialLease = nil
	}
	return driver, err
}

func persistenceError(id string, err error) error {
	return &SessionError{Kind: SessionErrorPersistence, SessionID: id, Operation: SessionOperationRegister, Detail: err.Error()}
}

// resolveInitialAgent binds the live session's active agent from the team. The
// AgentID is derived from the current config, never trusted from the caller.
func resolveInitialAgent(tm *team.Team, sess *session.Session) error {
	if sess.AgentName != "" {
		if _, _, ok := tm.ResolveLegacyName(sess.AgentName); !ok {
			return &SessionError{Kind: SessionErrorNotFound, SessionID: sess.ID, Operation: "resolve_agent_name"}
		}
	} else {
		a, err := tm.DefaultAgent()
		if err != nil {
			return err
		}
		sess.AgentName = a.Name()
	}
	sess.AgentID = configuredAgentID(tm, sess.AgentName)
	return nil
}

// persistFirstTime writes the first row for an unregistered session before model
// resolution and publication. A row created by a concurrent writer is adopted, not
// rejected: the same creation rule as adoptStoredIdentity applies. It reports whether
// that adoption happened, so the caller re-resolves the active agent. Nothing is
// deleted on failure, and a failed revision write leaves the row in place.
func (g *sessionDriverRegistry) persistFirstTime(ctx context.Context, sess *session.Session, tm *team.Team) (bool, error) {
	if g.r == nil || g.r.sessionStore == nil {
		return false, nil
	}
	g.mu.Lock()
	_, deleted := g.deleted[sess.ID]
	_, stopped := g.stoppedTrees[sess.ID]
	blocked := g.closed || deleted || stopped || g.reservations[sess.ID] != nil
	g.mu.Unlock()
	if blocked {
		// Publication reports the terminal or reservation error; no row is written.
		return false, nil
	}
	creation := sess.AttributesSnapshot()[SessionAgentAttribute]
	if creation == "" {
		creation = sess.AgentName
	}
	row := sess.OwnSnapshot()
	// The transcript is written by the run's own persistence, never by this first row.
	row.Messages = nil
	if creation != "" {
		row.SetAttribute(SessionAgentAttribute, creation)
		row.SetAttribute(SessionAgentIDAttribute, configuredAgentID(tm, creation))
	}
	store := g.r.sessionStore
	if err := store.AddSession(ctx, row); err != nil {
		if !errors.Is(err, session.ErrAlreadyExists) {
			return false, persistenceError(sess.ID, err)
		}
		found, adoptErr := g.adoptStoredIdentity(ctx, sess)
		if adoptErr != nil {
			return false, adoptErr
		}
		if !found {
			return false, persistenceError(sess.ID, errors.New("row disappeared during first write"))
		}
		return true, nil
	}
	if creation != "" {
		sess.SetAttribute(SessionAgentAttribute, creation)
		sess.SetAttribute(SessionAgentIDAttribute, configuredAgentID(tm, creation))
	}
	if err := recordTeamRevision(ctx, store, tm, row, creation); err != nil {
		return false, persistenceError(sess.ID, err)
	}
	return false, nil
}

// canonicalAttributes are the creation and provenance keys an existing row owns.
var canonicalAttributes = []string{
	SessionAgentAttribute,
	SessionAgentIDAttribute,
	SessionParentAgentAttribute,
	SessionParentAgentIDAttribute,
	"docker-agent.actor.source",
}

// adoptStoredIdentity makes an existing row's canonical identity the live
// session's, before name and model resolution, and reports whether the row exists.
// Creation and provenance attributes, origin, parent, workspace, active agent and
// title are adopted; transcript and other caller settings are kept. A template
// creation agent that differs from the stored one is a conflict.
func (g *sessionDriverRegistry) adoptStoredIdentity(ctx context.Context, sess *session.Session) (bool, error) {
	if g.r == nil || g.r.sessionStore == nil {
		return false, nil
	}
	stored, err := session.ReadSessionMetadata(ctx, g.r.sessionStore, sess.ID)
	if errors.Is(err, session.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, persistenceError(sess.ID, err)
	}
	attrs := stored.AttributesSnapshot()
	template := sess.AttributesSnapshot()[SessionAgentAttribute]
	if template != "" && attrs[SessionAgentAttribute] != "" && template != attrs[SessionAgentAttribute] {
		return false, &SessionError{Kind: SessionErrorConflict, SessionID: sess.ID, Operation: SessionOperationRegister, Detail: "creation agent differs from stored session"}
	}
	for _, key := range canonicalAttributes {
		if value, ok := attrs[key]; ok {
			sess.SetAttribute(key, value)
		} else {
			sess.DeleteAttribute(key)
		}
	}
	sess.ParentID = stored.ParentID
	if stored.WorkingDir != "" {
		sess.WorkingDir = stored.WorkingDir
	}
	// A nonempty caller title is live state; the stored title is only a fallback.
	if sess.Title == "" && stored.Title != "" {
		sess.Title = stored.Title
	}
	if stored.AgentName != "" {
		sess.AgentName = stored.AgentName
	}
	return true, nil
}

// markRemoval records a removal. A final removal takes the ID's initialization
// fence first, so an in-flight first-time save completes before the tombstone or
// observes it. The fence wait honors ctx; a timed-out removal changes nothing.
func (g *sessionDriverRegistry) markRemoval(ctx context.Context, sessionID string, expected *sessionDriver, final bool) (*sessionDriver, bool, error) {
	if final {
		fence := &g.initializing[maphash.String(g.publicationSeed, sessionID)%uint64(len(g.initializing))]
		if err := lockFence(ctx, fence); err != nil {
			return nil, false, err
		}
		defer fence.Unlock()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.drivers[sessionID]
	if expected != nil && d != expected {
		return nil, true, nil
	}
	if d != nil {
		d.maintenanceRetired = true
	}
	if final {
		g.deleted[sessionID] = struct{}{}
	}
	return d, false, nil
}

// lockFence acquires mu or returns ctx's error. An abandoned acquisition is
// released by a helper goroutine once it succeeds, so no waiter leaks the fence.
func lockFence(ctx context.Context, mu *sync.Mutex) error {
	acquired := make(chan struct{})
	go func() {
		mu.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return nil
	case <-ctx.Done():
		go func() {
			<-acquired
			mu.Unlock()
		}()
		return ctx.Err()
	}
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
	return g.publishInitializedWithBindingAndEnvironment(context.Background(), sess, modelRef, providers, agentName, maxIterations, stampAgent, bind, nil)
}

func (g *sessionDriverRegistry) publishInitializedWithBindingAndEnvironment(ctx context.Context, sess *session.Session, modelRef string, providers []provider.Provider, agentName string, maxIterations int, stampAgent, bind bool, pinned *TeamEnvironmentLease) (*sessionDriver, error) {
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
	var initialLease *TeamEnvironmentLease
	if pinned != nil {
		initialLease = pinned
	}
	if g.r != nil && g.r.teamEnvironments != nil && initialLease == nil {
		var leaseErr error
		if env := teamEnvironmentFromContext(ctx); env != nil {
			initialLease, leaseErr = g.r.teamEnvironments.Acquire(env.RevisionID())
		} else {
			initialLease, leaseErr = g.r.teamEnvironments.AcquireCurrent()
		}
		if leaseErr != nil {
			return nil, leaseErr
		}
		defer func() {
			if initialLease != nil && initialLease != pinned {
				_ = initialLease.Release(context.WithoutCancel(g.r.lifetime()))
			}
		}()
	}
	currentTeam := g.r.team
	if initialLease != nil {
		currentTeam = initialLease.Environment().Team()
	}
	if !bind && sess.AgentName == "" && currentTeam != nil {
		defaultAgent, err := currentTeam.DefaultAgent()
		if err != nil {
			return nil, err
		}
		sess.AgentName = defaultAgent.Name()
		sess.AgentID = configuredAgentID(currentTeam, sess.AgentName)
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
				sess.SetAttribute(SessionAgentIDAttribute, configuredAgentID(currentTeam, agentName))
			}
			if currentTeam != nil {
				sess.AgentID = configuredAgentID(currentTeam, sess.AgentName)
			}
		}
		d = newSessionDriver(g.r, sess)
		g.mu.Unlock()
		if initialLease != nil || pinned != nil || g.r != nil && g.r.teamEnvironments != nil {
			lease := pinned
			if lease == nil {
				lease = initialLease
			}
			if lease == nil {
				var leaseErr error
				lease, leaseErr = g.r.teamEnvironments.AcquireCurrent()
				if leaseErr != nil {
					d.closeOwner()
					return nil, leaseErr
				}
			}
			if err := d.ownerCall(context.WithoutCancel(g.r.lifetime()), func() error { d.teamEnvironment = lease; return nil }); err != nil {
				_ = lease.Release(context.WithoutCancel(g.r.lifetime()))
				d.closeOwner()
				return nil, err
			}
			if lease == initialLease {
				initialLease = nil
			}
		}
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
	// done closes exactly once when the reservation leaves the registry, on
	// publish, discard, or registry shutdown. A colliding prepare waits on it.
	done       chan struct{}
	settleOnce sync.Once
}

func (r *restoreDriverReservation) settle() {
	if r == nil || r.done == nil {
		return
	}
	r.settleOnce.Do(func() { close(r.done) })
}

// reservationFor returns the in-flight reservation for id, or nil.
func (g *sessionDriverRegistry) reservationFor(id string) *restoreDriverReservation {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reservations[id]
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
	tm := g.r.team
	var initialLease *TeamEnvironmentLease
	if g.r.teamEnvironments != nil {
		var leaseErr error
		if env := teamEnvironmentFromContext(ctx); env != nil {
			initialLease, leaseErr = g.r.teamEnvironments.Acquire(env.RevisionID())
		} else {
			initialLease, leaseErr = g.r.teamEnvironments.AcquireCurrent()
		}
		if leaseErr != nil {
			return nil, leaseErr
		}
		defer func() { _ = initialLease.Release(context.WithoutCancel(g.r.lifetime())) }()
		tm = initialLease.Environment().Team()
	}
	if tm != nil {
		if sess.AgentID != "" || sess.AgentName != "" {
			if _, ok := bindExecutionIdentity(tm, sess); !ok {
				operation := "resolve_agent_name"
				if sess.AgentID != "" {
					operation = "resolve_agent_id"
				}
				return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: sess.ID, Operation: SessionOperation(operation)}
			}
		} else {
			a, err := tm.DefaultAgent()
			if err != nil {
				return nil, err
			}
			sess.AgentName = a.Name()
			sess.AgentID = configuredAgentID(tm, a.Name())
		}
		var err error
		modelRef, providers, err = g.r.resolveSessionModelBinding(environmentScopedContext(ctx, initialLease), sess, "")
		if err != nil {
			return nil, err
		}
	}
	d := newSessionDriver(g.r, sess)
	if initialLease != nil {
		if err := d.ownerCall(ctx, func() error { d.teamEnvironment = initialLease; return nil }); err != nil {
			_ = initialLease.Release(context.WithoutCancel(g.r.lifetime()))
			d.closeOwner()
			return nil, err
		}
		initialLease = nil
	}
	d.SetModelBinding(modelRef, providers)
	reservation := &restoreDriverReservation{registry: g, driver: d, id: sess.ID, replaces: replaces, done: make(chan struct{})}
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
	defer r.settle()
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
		reservation.settle()
	}
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
	return d.replaceSession(sess) == nil
}

func (g *sessionDriverRegistry) ReleaseDriver(ctx context.Context, sessionID string, expected *sessionDriver) error {
	return g.remove(ctx, sessionID, expected, false)
}

func (g *sessionDriverRegistry) remove(ctx context.Context, sessionID string, expected *sessionDriver, final bool) error {
	d, skip, err := g.markRemoval(ctx, sessionID, expected, final)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}
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
		g.reservations[id].settle()
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

// restoreReserved reports whether a restore currently reserves id. It takes the
// registry lock only briefly and never mutates state.
func (g *sessionDriverRegistry) restoreReserved(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reservations[id] != nil
}
