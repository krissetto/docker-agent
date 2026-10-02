package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// SessionRuntimeFactory builds the session runtime that executes sessions of one
// agent source rooted at workingDir (empty means the server's own working directory).
type SessionRuntimeFactory func(ctx context.Context, source config.Source, workingDir string) (runtime.SessionRuntimeSupervisor, error)

func WithSessionRuntimeFactory(factory SessionRuntimeFactory) SessionManagerOpt {
	return func(sm *SessionManager) { sm.sessionRuntimeFactory = factory }
}

type sourceGeneration interface{ Generation() uint64 }

type workspaceKey struct {
	dir        string
	generation uint64
}

const (
	defaultWorkspaceRuntimeIdleTTL = 15 * time.Minute
	defaultWorkspaceRuntimeIdleCap = 8
)

type workspaceRuntimeEntry struct {
	supervisor runtime.SessionRuntimeSupervisor
	runtime    runtime.SessionRuntime
	lastUsed   time.Time
	refs       int // indexed live/loaded sessions plus in-flight router operations
}

type workspaceBuild struct {
	done    chan struct{}
	err     error
	waiters int
}

// workspaceSessionRuntimes routes sessions by workspace and source generation.
// The cap applies to idle entries only: an entry with a router-visible loaded
// session is retained regardless of the cap, because shutting it down would
// invalidate a live session. Sessions restored internally by a runtime have no
// callback into this router; misses therefore retain a safe scan fallback and
// only entries known to have no owners are eligible for eviction.
type workspaceSessionRuntimes struct {
	ctx    context.Context //nolint:containedctx // owns workspace runtime and pruner lifetimes
	source config.Source
	store  session.Store
	build  SessionRuntimeFactory

	mu            sync.Mutex
	runtimes      map[workspaceKey]*workspaceRuntimeEntry
	owners        map[string]workspaceKey
	building      map[workspaceKey]*workspaceBuild
	closed        bool
	idleTTL       time.Duration
	idleCap       int
	now           func() time.Time
	wakePrune     chan struct{}
	pruneDone     chan struct{}
	prunerStarted bool
	shutdownOnce  sync.Once
	shutdownDone  chan struct{}
	shutdownErr   error
	buildWG       sync.WaitGroup
	retired       []runtime.SessionRuntimeSupervisor // failed drains retained for final shutdown retry
}

var _ interface {
	runtime.SessionRuntime
	runtime.TreeInspector
	runtime.TreeRestorer
	runtime.AgentSwitcher
	runtime.SafetyDefaults
	runtime.SessionViewInfoReader
	runtime.SessionViewPreparer
} = (*workspaceSessionRuntimes)(nil)

func newWorkspaceSessionRuntimes(ctx context.Context, source config.Source, build SessionRuntimeFactory, stores ...session.Store) *workspaceSessionRuntimes {
	w := &workspaceSessionRuntimes{
		ctx: ctx, source: source, build: build,
		runtimes: map[workspaceKey]*workspaceRuntimeEntry{}, owners: map[string]workspaceKey{}, building: map[workspaceKey]*workspaceBuild{},
		idleTTL: defaultWorkspaceRuntimeIdleTTL, idleCap: defaultWorkspaceRuntimeIdleCap, now: time.Now,
		wakePrune: make(chan struct{}, 1), pruneDone: make(chan struct{}), shutdownDone: make(chan struct{}),
	}
	if len(stores) != 0 {
		w.store = stores[0]
	}
	return w
}

func (w *workspaceSessionRuntimes) configureIdlePolicy(ttl time.Duration, capacity int, now func() time.Time) {
	w.mu.Lock()
	w.idleTTL = ttl
	w.idleCap = max(capacity, 0)
	if now != nil {
		w.now = now
	}
	w.mu.Unlock()
	w.wakePruner()
}

func (w *workspaceSessionRuntimes) startPruner() {
	go w.pruneLoop()
	go func() {
		<-w.ctx.Done()
		_ = w.Shutdown(context.WithoutCancel(w.ctx))
	}()
}

func (w *workspaceSessionRuntimes) wakePruner() {
	select {
	case w.wakePrune <- struct{}{}:
	default:
	}
}

func (w *workspaceSessionRuntimes) pruneLoop() {
	var timer *time.Timer
	for {
		w.mu.Lock()
		closed := w.closed
		var wait time.Duration
		haveDeadline := false
		if w.idleTTL >= 0 {
			now := w.now()
			for _, entry := range w.runtimes {
				if entry.refs != 0 {
					continue
				}
				candidate := max(entry.lastUsed.Add(w.idleTTL).Sub(now), 0)
				if !haveDeadline || candidate < wait {
					wait, haveDeadline = candidate, true
				}
			}
		}
		w.mu.Unlock()
		if closed {
			if timer != nil {
				timer.Stop()
			}
			close(w.pruneDone)
			return
		}
		var timerC <-chan time.Time
		if haveDeadline {
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(wait)
			}
			timerC = timer.C
		}
		select {
		case <-w.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			close(w.pruneDone)
			return
		case <-w.wakePrune:
		case <-timerC:
			w.prune()
		}
	}
}

func (w *workspaceSessionRuntimes) currentGeneration() uint64 {
	if g, ok := w.source.(sourceGeneration); ok {
		return g.Generation()
	}
	return 0
}

func normalizeDir(dir string) string {
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return filepath.Clean(dir)
}

// acquireRuntime coordinates construction per key without holding the global
// router lock while the potentially expensive factory runs. The returned
// release must be called after the operation has either published ownership or
// failed.
func (w *workspaceSessionRuntimes) acquireRuntime(ctx context.Context, dir string) (runtime.SessionRuntime, workspaceKey, func(), error) {
	key := workspaceKey{dir: normalizeDir(dir), generation: w.currentGeneration()}
	if err := ctx.Err(); err != nil {
		return nil, key, func() {}, err
	}
	w.mu.Lock()
	if w.closed || w.ctx.Err() != nil {
		w.mu.Unlock()
		return nil, key, func() {}, runtime.ErrSessionClosed
	}
	if entry := w.runtimes[key]; entry != nil {
		entry.refs++
		entry.lastUsed = w.now()
		w.mu.Unlock()
		return entry.runtime, key, func() { w.releaseRef(key) }, nil
	}
	call := w.building[key]
	if call == nil {
		call = &workspaceBuild{done: make(chan struct{})}
		w.building[key] = call
		w.buildWG.Add(1)
		go w.buildRuntime(key, call)
	}
	call.waiters++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		call.waiters--
		// Construction pins its result until the last original waiter has
		// either acquired its own lease or canceled.
		finished := false
		select {
		case <-call.done:
			finished = true
		default:
		}
		release := finished && call.err == nil && call.waiters == 0
		w.mu.Unlock()
		if release {
			w.releaseRef(key)
		}
	}()
	select {
	case <-ctx.Done():
		return nil, key, func() {}, ctx.Err()
	case <-w.ctx.Done():
		return nil, key, func() {}, runtime.ErrSessionClosed
	case <-call.done:
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, key, func() {}, err
	}
	if call.err != nil {
		return nil, key, func() {}, call.err
	}
	entry := w.runtimes[key]
	if w.closed || entry == nil {
		return nil, key, func() {}, runtime.ErrSessionClosed
	}
	entry.refs++
	entry.lastUsed = w.now()
	return entry.runtime, key, func() { w.releaseRef(key) }, nil
}

func (w *workspaceSessionRuntimes) buildRuntime(key workspaceKey, call *workspaceBuild) {
	defer w.buildWG.Done()
	supervisor, err := w.build(w.ctx, w.source, key.dir)
	if err != nil {
		err = fmt.Errorf("creating session runtime for %q in %q: %w", w.source.Name(), key.dir, err)
	}
	w.mu.Lock()
	delete(w.building, key)
	if err == nil && !w.closed && w.ctx.Err() == nil {
		startPruner := !w.prunerStarted
		w.prunerStarted = true
		refs := 0
		if call.waiters != 0 {
			refs = 1
		}
		w.runtimes[key] = &workspaceRuntimeEntry{supervisor: supervisor, runtime: supervisor.Runtime(), lastUsed: w.now(), refs: refs}
		close(call.done)
		w.mu.Unlock()
		if startPruner {
			w.startPruner()
		}
		w.prune()
		return
	}
	if err == nil {
		err = runtime.ErrSessionClosed
	}
	call.err = err
	close(call.done)
	w.mu.Unlock()
	if supervisor != nil {
		w.retire(supervisor)
	}
}

// retire retains ownership on drain failure. Final router shutdown retries it
// once; no background retry loop can grow without bound or race a live owner.
func (w *workspaceSessionRuntimes) retire(supervisor runtime.SessionRuntimeSupervisor) {
	if err := supervisor.Shutdown(context.WithoutCancel(w.ctx)); err != nil {
		slog.ErrorContext(w.ctx, "Failed to retire idle session runtime", "source", w.source.Name(), "error", err)
		w.mu.Lock()
		w.retired = append(w.retired, supervisor)
		w.mu.Unlock()
	}
}

func (w *workspaceSessionRuntimes) releaseRef(key workspaceKey) {
	w.mu.Lock()
	if entry := w.runtimes[key]; entry != nil {
		entry.refs--
		entry.lastUsed = w.now()
	}
	w.mu.Unlock()
	w.prune()
	w.wakePruner()
}

func (w *workspaceSessionRuntimes) remember(sessionID string, key workspaceKey) {
	if sessionID == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, exists := w.owners[sessionID]; !exists {
		if entry := w.runtimes[key]; entry != nil {
			entry.refs++
			w.owners[sessionID] = key
		}
	}
}

func (w *workspaceSessionRuntimes) forget(sessionID string) {
	w.mu.Lock()
	if key, ok := w.owners[sessionID]; ok {
		delete(w.owners, sessionID)
		if entry := w.runtimes[key]; entry != nil {
			entry.refs--
			entry.lastUsed = w.now()
		}
	}
	w.mu.Unlock()
	w.prune()
	w.wakePruner()
}

func (w *workspaceSessionRuntimes) prune() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	now, generation := w.now(), w.currentGeneration()
	idle := make([]workspaceKey, 0)
	for key, entry := range w.runtimes {
		if entry.refs == 0 {
			idle = append(idle, key)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return w.runtimes[idle[i]].lastUsed.Before(w.runtimes[idle[j]].lastUsed) })
	remove := map[workspaceKey]bool{}
	for _, key := range idle {
		entry := w.runtimes[key]
		if key.generation != generation || (w.idleTTL >= 0 && now.Sub(entry.lastUsed) >= w.idleTTL) {
			remove[key] = true
		}
	}
	remaining := len(idle) - len(remove)
	capacity := max(w.idleCap, 0)
	for _, key := range idle {
		if remaining <= capacity {
			break
		}
		if !remove[key] {
			remove[key] = true
			remaining--
		}
	}
	retired := make([]runtime.SessionRuntimeSupervisor, 0, len(remove))
	for key := range remove {
		retired = append(retired, w.runtimes[key].supervisor)
		delete(w.runtimes, key)
	}
	// Add under the same lock that fences shutdown so its Wait cannot miss
	// a retirement whose failed owner has not yet reached the retained list.
	if len(retired) != 0 {
		w.buildWG.Add(1)
	}
	w.mu.Unlock()
	if len(retired) != 0 {
		defer w.buildWG.Done()
	}
	for _, supervisor := range retired {
		w.retire(supervisor)
	}
	w.wakePruner()
}

// owner uses the direct index first. A scan remains necessary for child
// sessions restored internally by a runtime; successful fallback lookups seed
// the index so subsequent operations are O(1).
func (w *workspaceSessionRuntimes) owner(sessionID string) (runtime.SessionRuntime, workspaceKey, bool) {
	w.mu.Lock()
	if key, ok := w.owners[sessionID]; ok {
		if entry := w.runtimes[key]; entry != nil {
			entry.lastUsed = w.now()
			rt := entry.runtime
			w.mu.Unlock()
			return rt, key, true
		}
		delete(w.owners, sessionID)
	}
	type candidate struct {
		key workspaceKey
		rt  runtime.SessionRuntime
	}
	candidates := make([]candidate, 0, len(w.runtimes))
	for key, entry := range w.runtimes {
		entry.refs++ // scan lease: an uncertain entry cannot be retired mid-lookup
		candidates = append(candidates, candidate{key, entry.runtime})
	}
	w.mu.Unlock()
	for i, candidate := range candidates {
		if _, err := candidate.rt.SessionByID(sessionID); err == nil {
			w.remember(sessionID, candidate.key)
			for _, leased := range candidates[i:] {
				w.releaseRef(leased.key)
			}
			for _, leased := range candidates[:i] {
				w.releaseRef(leased.key)
			}
			return candidate.rt, candidate.key, true
		}
	}
	for _, candidate := range candidates {
		w.releaseRef(candidate.key)
	}
	return nil, workspaceKey{}, false
}

func (w *workspaceSessionRuntimes) CreateSession(ctx context.Context, sess *session.Session, binding runtime.SessionBinding) (runtime.SessionHandle, error) {
	if sess == nil {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, Operation: "create_session"}
	}
	if id := binding.ParentSessionID; id != "" {
		rt, key, ok := w.owner(id)
		if !ok {
			return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "create_parent"}
		}
		handle, err := rt.CreateSession(ctx, sess, binding)
		if err == nil {
			w.remember(handle.ID(), key)
		}
		return handle, err
	}
	if rt, key, ok := w.owner(sess.ID); ok {
		handle, err := rt.CreateSession(ctx, sess, binding)
		if err == nil {
			w.remember(handle.ID(), key)
		}
		return handle, err
	}
	rt, key, release, err := w.acquireRuntime(ctx, sess.WorkingDir)
	if err != nil {
		return nil, err
	}
	defer release()
	handle, err := rt.CreateSession(ctx, sess, binding)
	if err == nil {
		w.remember(handle.ID(), key)
	}
	return handle, err
}

func (w *workspaceSessionRuntimes) SessionByID(sessionID string) (runtime.SessionHandle, error) {
	rt, _, ok := w.owner(sessionID)
	if !ok {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: sessionID, Operation: "lookup"}
	}
	return rt.SessionByID(sessionID)
}

func (w *workspaceSessionRuntimes) DeleteSession(ctx context.Context, sessionID string) error {
	var rt runtime.SessionRuntime
	var release func()
	var err error
	if w.store != nil {
		rt, _, release, err = w.acquireViewRuntime(ctx, sessionID)
	} else if owner, _, ok := w.owner(sessionID); ok {
		rt, release = owner, func() {}
	} else {
		rt, _, release, err = w.acquireRuntime(ctx, "")
	}
	if err != nil {
		return err
	}
	defer release()
	if err := rt.DeleteSession(ctx, sessionID); err != nil {
		return err
	}
	w.forget(sessionID)
	// The runtime owns the cascade. Only canonical absence in both runtime
	// and durable storage proves a cached descendant was deleted; a released
	// driver alone must never unpin other restored sessions in its workspace.
	w.mu.Lock()
	ids := make([]string, 0, len(w.owners))
	for id, key := range w.owners {
		if entry := w.runtimes[key]; entry != nil && entry.runtime == rt {
			ids = append(ids, id)
		}
	}
	w.mu.Unlock()
	for _, id := range ids {
		if canonicalSessionDeleted(context.WithoutCancel(ctx), rt, w.store, id) {
			w.forget(id)
		}
	}
	return nil
}

func (w *workspaceSessionRuntimes) InspectSessionTree(ctx context.Context, rootSessionID string) (*subagent.Snapshot, error) {
	rt, _, release, err := w.acquireViewRuntime(ctx, rootSessionID)
	if err != nil {
		return nil, err
	}
	defer release()
	inspector, ok := rt.(runtime.TreeInspector)
	if !ok {
		return nil, errors.New("runtime cannot inspect durable child tree")
	}
	return inspector.InspectSessionTree(ctx, rootSessionID)
}

func (w *workspaceSessionRuntimes) RestoreSessionTree(ctx context.Context, root *session.Session) error {
	if root == nil {
		return &runtime.SessionError{Kind: runtime.SessionErrorInvalid, Operation: "restore_tree"}
	}
	rt, key, ok := w.owner(root.ID)
	var release func()
	if !ok {
		var err error
		rt, key, release, err = w.acquireRuntime(ctx, root.WorkingDir)
		if err != nil {
			return err
		}
		defer release()
	}
	restorer, ok := rt.(runtime.TreeRestorer)
	if !ok {
		return &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: root.ID, Operation: "restore_child_tree"}
	}
	if err := restorer.RestoreSessionTree(ctx, root); err != nil {
		return err
	}
	w.remember(root.ID, key)
	return nil
}

func (w *workspaceSessionRuntimes) SwitchAgent(ctx context.Context, sessionID, targetAgent string) (runtime.SessionHandle, *session.Session, error) {
	rt, key, ok := w.owner(sessionID)
	if !ok {
		return nil, nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: sessionID, Operation: "switch_agent"}
	}
	switcher, ok := rt.(runtime.AgentSwitcher)
	if !ok {
		return nil, nil, &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: sessionID, Operation: "switch_agent"}
	}
	handle, sess, err := switcher.SwitchAgent(ctx, sessionID, targetAgent)
	if err == nil {
		w.remember(handle.ID(), key)
	}
	return handle, sess, err
}

func (w *workspaceSessionRuntimes) AuthorSafetyDefault(sess *session.Session) session.SafetyPolicy {
	return w.authorSafetyDefault(w.ctx, sess)
}

func (w *workspaceSessionRuntimes) authorSafetyDefault(ctx context.Context, sess *session.Session) session.SafetyPolicy {
	if sess == nil {
		return ""
	}
	rt, _, release, err := w.acquireRuntime(ctx, sess.WorkingDir)
	if err != nil {
		return ""
	}
	defer release()
	if defaults, ok := rt.(runtime.SafetyDefaults); ok {
		return defaults.AuthorSafetyDefault(sess)
	}
	return ""
}

func (w *workspaceSessionRuntimes) runShutdown() {
	w.mu.Lock()
	w.closed = true
	started := w.prunerStarted
	w.mu.Unlock()
	if started {
		w.wakePruner()
		<-w.pruneDone
	}

	// Factories and idle retirements retain any failed drains before calling
	// Done, so this boundary covers all owned output.
	w.buildWG.Wait()

	w.mu.Lock()
	supervisors := w.retired
	w.retired = nil
	for _, entry := range w.runtimes {
		supervisors = append(supervisors, entry.supervisor)
	}
	w.runtimes = map[workspaceKey]*workspaceRuntimeEntry{}
	w.owners = map[string]workspaceKey{}
	w.mu.Unlock()

	shutdownCtx := context.WithoutCancel(w.ctx)
	var errs []error
	for _, supervisor := range supervisors {
		if err := supervisor.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "Failed to shut down session runtime", "source", w.source.Name(), "error", err)
			errs = append(errs, err)
			w.mu.Lock()
			w.retired = append(w.retired, supervisor)
			w.mu.Unlock()
		}
	}
	w.shutdownErr = errors.Join(errs...)
	close(w.shutdownDone)
}

func (w *workspaceSessionRuntimes) Shutdown(ctx context.Context) error {
	w.shutdownOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()
		go w.runShutdown()
	})
	select {
	case <-w.shutdownDone:
		return w.shutdownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Preserve the public context-free capability for embeddings, while HTTP
// requests can abandon a shared factory build when their caller disconnects.
func authorSafetyDefault(ctx context.Context, defaults runtime.SafetyDefaults, sess *session.Session) session.SafetyPolicy {
	if router, ok := defaults.(*workspaceSessionRuntimes); ok {
		return router.authorSafetyDefault(ctx, sess)
	}
	return defaults.AuthorSafetyDefault(sess)
}
