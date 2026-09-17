package supervisor

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
)

// DefaultMaxRetainedViewOwners bounds optional foreign view resource owners,
// not tabs, ordinary session admission, or the initial shared runtime.
const DefaultMaxRetainedViewOwners = 8

// ViewOwnerScope identifies one immutable source/configuration/authorization
// lifetime. Allocate it once at host bootstrap, never per factory invocation.
type ViewOwnerScope struct{ _ byte }

func NewViewOwnerScope() *ViewOwnerScope { return &ViewOwnerScope{} }

type ViewOwnerIdentity struct {
	Scope          *ViewOwnerScope
	Source         string
	RootSessionID  string
	RootWorkingDir string
	RootBinding    runtime.SessionBinding
}

// ViewOwnerResources contains reusable resources, not execution state. NewApp
// must be a pure app.NewResolved builder with the owner's captured options.
// Retain registers the accepted owner with the host transport; Cleanup includes
// unregister/drain and runs once, outside the supervisor lock.
type ViewOwnerResources struct {
	Services app.Services
	Sessions runtime.SessionRuntime
	NewApp   func(context.Context, runtime.CommittedSessionView) (*app.App, error)
	Cleanup  func()
	Retain   func() error
}

type HostViewConfig struct {
	Resolve               func(context.Context, string) (ViewOwnerIdentity, error)
	Factory               func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error)
	MaxRetainedViewOwners int
}

type SessionViewAcquirer interface {
	AcquireSessionView(ctx context.Context, sessionID string) (PreparedHostedView, error)
}

type PreparedHostedView interface {
	runtime.PreparedSessionView
	NewApp(ctx context.Context, committed runtime.CommittedSessionView) (*app.App, error)
}

type viewOwnerKey struct {
	scope  *ViewOwnerScope
	source string
	rootID string
}

func (i ViewOwnerIdentity) key() viewOwnerKey {
	return viewOwnerKey{i.Scope, i.Source, i.RootSessionID}
}

type viewOwner struct {
	identity       ViewOwnerIdentity
	resources      ViewOwnerResources
	ready          chan struct{}
	err            error
	refs           int
	initial        bool
	retained       bool
	safety         bool
	slot           bool
	removed        bool
	private        bool
	closeRequested bool
	redirect       *viewOwner
	registerOnce   sync.Once
	registerErr    error
}

type hostedView struct {
	supervisor *Supervisor
	owner      *viewOwner
	prepared   runtime.PreparedSessionView
	mu         sync.Mutex
	aborted    bool
	committed  bool
	committing bool
	stop       func() bool
	cancel     context.CancelFunc
	stopHost   func() bool
}

func ownerError(id string, kind runtime.SessionErrorKind, detail string) error {
	return &runtime.SessionError{Kind: kind, SessionID: id, Operation: "host_view", Detail: detail}
}

func sameSessionRuntime(a, b runtime.SessionRuntime) bool {
	return a != nil && b != nil && reflect.TypeOf(a).Comparable() && reflect.TypeOf(b).Comparable() && a == b
}

// ConfigureSessionViews installs the host seam once on the same Supervisor used
// for ordinary tabs. The host context, not a UI operation, owns factory lifetime.
func (s *Supervisor) ConfigureSessionViews(ctx context.Context, cfg HostViewConfig) error {
	if cfg.Resolve == nil || cfg.MaxRetainedViewOwners < 0 {
		return ownerError("", runtime.SessionErrorInvalid, "invalid host view configuration")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.viewConfig != nil {
		return ownerError("", runtime.SessionErrorClosed, "host closed or already configured")
	}
	hostCtx, cancel := context.WithCancel(ctx)
	s.viewContext = func() context.Context { return hostCtx }
	s.viewCancel = cancel
	s.viewConfig = &cfg
	s.viewOwners = make(map[viewOwnerKey]*viewOwner)
	s.viewLeases = make(map[*hostedView]struct{})
	return nil
}

// RegisterSessionOwner registers bootstrap or fresh ordinary resources BEFORE
// any persisted-root installation. Persisted loads use WithSessionOwner instead.
// Multiple root aliases of one actual runtime share one resource record.
func (s *Supervisor) RegisterSessionOwner(identity ViewOwnerIdentity, resources ViewOwnerResources, initial bool) error {
	if identity.Scope == nil || resources.Sessions == nil || !reflect.TypeOf(resources.Sessions).Comparable() || resources.NewApp == nil {
		return ownerError(identity.RootSessionID, runtime.SessionErrorInvalid, "owner requires canonical runtime identity and resolved-view builder")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.viewConfig == nil {
		return ownerError(identity.RootSessionID, runtime.SessionErrorClosed, "host unavailable")
	}
	if current := s.viewOwners[identity.key()]; current != nil {
		if sameSessionRuntime(current.resources.Sessions, resources.Sessions) {
			return nil
		}
		return ownerError(identity.RootSessionID, runtime.SessionErrorConflict, "root owner already admitted")
	}
	var owner *viewOwner
	for _, candidate := range s.ownerResources {
		if !candidate.removed && sameSessionRuntime(candidate.resources.Sessions, resources.Sessions) {
			if candidate.identity.Scope != identity.Scope || candidate.identity.Source != identity.Source {
				return ownerError(identity.RootSessionID, runtime.SessionErrorConflict, "resource provenance differs")
			}
			owner = candidate
			break
		}
	}
	if owner == nil {
		if resources.Cleanup != nil {
			resources.Cleanup = sync.OnceFunc(resources.Cleanup)
		}
		owner = &viewOwner{identity: identity, resources: resources, initial: initial, ready: make(chan struct{})}
		close(owner.ready)
		s.ownerResources = append(s.ownerResources, owner)
	}
	s.viewOwners[identity.key()] = owner
	// Registration can follow AddSession for compatibility, but never follows
	// a persisted load. Transfer the sole cleanup authority into this record.
	for _, runner := range s.runners {
		if runner.App != nil && sameSessionRuntime(runner.App.SessionRuntime(), resources.Sessions) {
			runner.owner = owner
			runner.cleanup = nil
		}
	}
	return nil
}

func (s *Supervisor) beginOwnerOperation() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.viewConfig == nil {
		return ownerError("", runtime.SessionErrorClosed, "host unavailable")
	}
	s.ownerOperations.Add(1)
	return nil
}

func (s *Supervisor) acquireOwner(ctx context.Context, id string, optional bool) (*viewOwner, error) {
	if err := s.beginOwnerOperation(); err != nil {
		return nil, err
	}
	defer s.ownerOperations.Done()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolveCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.viewContext(), cancel)
	defer func() { stop(); cancel() }()
	identity, err := s.viewConfig.Resolve(resolveCtx, id)
	if err != nil {
		return nil, err
	}
	return s.acquireOwnerIdentity(ctx, id, identity, optional)
}

func (s *Supervisor) acquireOwnerIdentity(ctx context.Context, id string, identity ViewOwnerIdentity, optional bool) (*viewOwner, error) {
	var err error
	if identity.Scope == nil || identity.RootSessionID == "" {
		return nil, ownerError(id, runtime.SessionErrorInvalid, "missing canonical root provenance")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, runtime.ErrSessionClosed
	}
	owner := s.viewOwners[identity.key()]
	if owner == nil {
		owner = &viewOwner{identity: identity, ready: make(chan struct{}), refs: 1, private: true}
		s.viewOwners[identity.key()] = owner
		s.ownerOperations.Add(1)
		go s.buildViewOwner(owner, id, optional)
	} else {
		owner.refs++
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		s.releaseOwner(owner)
		return nil, ctx.Err()
	case <-owner.ready:
	}
	s.mu.Lock()
	// A discovery reservation may have become an alias of another resource.
	actual := s.viewOwners[identity.key()]
	if owner.err != nil || s.closed || actual == nil {
		err = owner.err
		if err == nil {
			err = runtime.ErrSessionClosed
		}
		s.mu.Unlock()
		s.releaseOwner(owner)
		return nil, err
	}
	if actual != owner {
		owner.refs--
		owner = actual
	}
	if optional && !owner.initial && !owner.retained && !owner.slot {
		if s.retainedOwnerCountLocked() >= s.viewConfig.MaxRetainedViewOwners {
			s.mu.Unlock()
			s.releaseOwner(owner)
			return nil, &runtime.SessionError{Kind: runtime.SessionErrorCapacity, Operation: "host_view", Reason: runtime.SessionErrorReasonLimit, Limit: s.viewConfig.MaxRetainedViewOwners}
		}
		owner.slot = true
	}
	s.mu.Unlock()
	return owner, nil
}

// buildViewOwner discovers ALL admitted active/hidden/retained resources before
// reserving optional capacity and invoking the factory. No runtime call is made
// under mu. The keyed placeholder fences ordinary persisted admission as well.
func (s *Supervisor) buildViewOwner(pending *viewOwner, selectedID string, optional bool) {
	defer s.ownerOperations.Done()
	identity := pending.identity
	s.mu.Lock()
	candidates := make([]*viewOwner, 0, len(s.ownerResources))
	for _, candidate := range s.ownerResources {
		if !candidate.removed && candidate.resources.Sessions != nil && candidate.identity.Scope == identity.Scope && candidate.identity.Source == identity.Source {
			candidate.refs++
			candidates = append(candidates, candidate)
		}
	}
	s.mu.Unlock()
	var winner *viewOwner
	var err error
	for _, candidate := range candidates {
		handle, lookupErr := candidate.resources.Sessions.SessionByID(identity.RootSessionID)
		if lookupErr != nil {
			var sessionErr *runtime.SessionError
			if !errors.As(lookupErr, &sessionErr) || sessionErr.Kind != runtime.SessionErrorNotFound {
				err = lookupErr
				break
			}
		}
		if lookupErr != nil || handle == nil {
			handle, lookupErr = candidate.resources.Sessions.SessionByID(selectedID)
			if lookupErr != nil {
				var sessionErr *runtime.SessionError
				if !errors.As(lookupErr, &sessionErr) || sessionErr.Kind != runtime.SessionErrorNotFound {
					err = lookupErr
					break
				}
			}
		}
		if lookupErr == nil && handle != nil {
			if winner != nil && winner != candidate {
				err = ownerError(selectedID, runtime.SessionErrorConflict, "multiple canonical resource owners")
				break
			}
			winner = candidate
		}
	}
	// The initial shared owner can admit another root in its configured
	// workspace. Capacity errors in that runtime must never spawn a bypass.
	if winner == nil && err == nil {
		for _, candidate := range candidates {
			if candidate.initial && candidate.identity.RootWorkingDir == identity.RootWorkingDir {
				if winner != nil && winner != candidate {
					err = ownerError(selectedID, runtime.SessionErrorConflict, "ambiguous shared owner")
					break
				}
				winner = candidate
			}
		}
	}
	s.mu.Lock()
	if s.closed || pending.removed {
		err = runtime.ErrSessionClosed
	}
	if err == nil && winner != nil {
		pending.redirect = winner
		winner.refs += pending.refs
		s.viewOwners[identity.key()] = winner
	} else if err == nil {
		switch {
		case optional && s.retainedOwnerCountLocked() >= s.viewConfig.MaxRetainedViewOwners:
			err = &runtime.SessionError{Kind: runtime.SessionErrorCapacity, Operation: "host_view", Reason: runtime.SessionErrorReasonLimit, Limit: s.viewConfig.MaxRetainedViewOwners}
		case s.viewConfig.Factory == nil:
			err = ownerError(selectedID, runtime.SessionErrorUnsupported, "foreign workspace acquisition unavailable")
		default:
			pending.slot = optional
			s.ownerResources = append(s.ownerResources, pending)
		}
	}
	s.mu.Unlock()
	for _, candidate := range candidates {
		s.releaseOwner(candidate)
	}
	if winner == nil && err == nil {
		resources, factoryErr := s.viewConfig.Factory(s.viewContext(), identity)
		if resources.Cleanup != nil {
			resources.Cleanup = sync.OnceFunc(resources.Cleanup)
		}
		err = factoryErr
		if err == nil && (resources.Sessions == nil || resources.NewApp == nil || !reflect.TypeOf(resources.Sessions).Comparable()) {
			err = ownerError(selectedID, runtime.SessionErrorInvalid, "invalid owner factory resources")
		}
		s.mu.Lock()
		if s.closed || pending.removed {
			err = runtime.ErrSessionClosed
		}
		if err == nil {
			pending.resources = resources
		}
		s.mu.Unlock()
		if err != nil && resources.Cleanup != nil {
			resources.Cleanup()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending.err = err
	if err != nil {
		s.removeOwnerLocked(pending)
	}
	close(pending.ready)
}

func (s *Supervisor) retainedOwnerCountLocked() int {
	count := 0
	for _, owner := range s.ownerResources {
		if !owner.initial && owner.slot {
			count++
		}
	}
	return count
}

func (s *Supervisor) removeOwnerLocked(owner *viewOwner) {
	owner.removed = true
	for key, current := range s.viewOwners {
		if current == owner {
			delete(s.viewOwners, key)
		}
	}
	s.ownerResources = removeOwner(s.ownerResources, owner)
}

func removeOwner(owners []*viewOwner, owner *viewOwner) []*viewOwner {
	for i, candidate := range owners {
		if candidate == owner {
			return append(owners[:i], owners[i+1:]...)
		}
	}
	return owners
}

func (s *Supervisor) ownerHasRunnerLocked(owner *viewOwner) bool {
	for _, runner := range s.runners {
		if runner.owner == owner {
			return true
		}
	}
	return false
}

func (s *Supervisor) releaseOwner(owner *viewOwner) {
	s.mu.Lock()
	owner.refs--
	if owner.redirect != nil {
		redirect := owner.redirect
		s.mu.Unlock()
		s.releaseOwner(redirect)
		return
	}
	var cleanup func()
	if !s.closed && owner.refs == 0 && owner.private && owner.resources.Sessions == nil {
		// A late factory still occupies its reserved slot until it returns and
		// disposes its private resources. Cancellation cannot overbook factories.
		owner.removed = true
		for key, current := range s.viewOwners {
			if current == owner {
				delete(s.viewOwners, key)
			}
		}
		s.mu.Unlock()
		return
	}
	if owner.refs == 0 && !owner.retained {
		owner.slot = false
	}
	if !s.closed && owner.refs == 0 && (owner.private || owner.closeRequested) && !owner.retained && !owner.initial && !s.ownerHasRunnerLocked(owner) {
		s.removeOwnerLocked(owner)
		cleanup = owner.resources.Cleanup
	}
	s.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

// acceptOwner is the host ownership linearization point, BEFORE any operation
// that may publish canonical execution. An error from that operation is not a
// proof of nonpublication and cannot revoke this retained owner.
func (s *Supervisor) acceptOwner(owner *viewOwner, optional bool) error {
	s.mu.Lock()
	if s.closed || owner.removed {
		s.mu.Unlock()
		return runtime.ErrSessionClosed
	}
	if !owner.initial && !owner.retained && !owner.slot && optional {
		if s.retainedOwnerCountLocked() >= s.viewConfig.MaxRetainedViewOwners {
			s.mu.Unlock()
			return &runtime.SessionError{Kind: runtime.SessionErrorCapacity, Operation: "host_view", Reason: runtime.SessionErrorReasonLimit, Limit: s.viewConfig.MaxRetainedViewOwners}
		}
		owner.slot = true
	}
	owner.retained = true
	if !optional && !owner.initial {
		owner.slot = true
	}
	s.mu.Unlock()
	owner.registerOnce.Do(func() {
		if owner.resources.Retain != nil {
			owner.registerErr = owner.resources.Retain()
		}
	})
	return owner.registerErr
}

func (s *Supervisor) AcquireSessionView(ctx context.Context, id string) (PreparedHostedView, error) {
	owner, err := s.acquireOwner(ctx, id, true)
	if err != nil {
		return nil, err
	}
	if err := s.beginOwnerOperation(); err != nil {
		s.releaseOwner(owner)
		return nil, err
	}
	defer s.ownerOperations.Done()
	if err := ctx.Err(); err != nil {
		s.releaseOwner(owner)
		return nil, err
	}
	preparer, ok := owner.resources.Sessions.(runtime.SessionViewPreparer)
	if !ok {
		s.releaseOwner(owner)
		return nil, ownerError(id, runtime.SessionErrorUnsupported, "owner cannot prepare session views")
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.viewContext(), cancel)
	prepared, err := preparer.PrepareSessionView(opCtx, id)
	if err != nil {
		stop()
		cancel()
		s.releaseOwner(owner)
		return nil, err
	}
	view := &hostedView{supervisor: s, owner: owner, prepared: prepared, cancel: cancel, stopHost: stop}
	s.mu.Lock()
	if s.closed || ctx.Err() != nil {
		s.mu.Unlock()
		stop()
		cancel()
		prepared.Abort()
		s.releaseOwner(owner)
		return nil, runtime.ErrSessionClosed
	}
	s.viewLeases[view] = struct{}{}
	s.mu.Unlock()
	view.mu.Lock()
	defer view.mu.Unlock()
	view.stop = context.AfterFunc(ctx, view.Abort)
	return view, nil
}

// WithSessionOwner is the same admission fence for ordinary persisted loading
// and control-plane cold admission. It does not change ordinary execution or
// impose the optional view-retention cap on existing safety ownership.
func (s *Supervisor) WithSessionOwner(ctx context.Context, id string, use func(context.Context, ViewOwnerResources) error) error {
	owner, err := s.acquireOwner(ctx, id, false)
	if err != nil {
		return err
	}
	defer s.releaseOwner(owner)
	if err := s.beginOwnerOperation(); err != nil {
		return err
	}
	defer s.ownerOperations.Done()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.acceptOwner(owner, false); err != nil {
		return err
	}
	return use(ctx, owner.resources)
}

func (v *hostedView) Info() runtime.PreparedSessionViewInfo { return v.prepared.Info() }

func (v *hostedView) pinOperation(requireCommitted bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.aborted || (requireCommitted && !v.committed) {
		return runtime.ErrSessionClosed
	}
	if !requireCommitted && v.committing {
		return ownerError("", runtime.SessionErrorConflict, "commit already in progress")
	}
	s := v.supervisor
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || v.owner.removed {
		return runtime.ErrSessionClosed
	}
	v.owner.refs++
	s.ownerOperations.Add(1)
	if !requireCommitted {
		v.committing = true
	}
	return nil
}

func (v *hostedView) finishOperation() {
	v.supervisor.releaseOwner(v.owner)
	v.supervisor.ownerOperations.Done()
}

func (v *hostedView) Commit(ctx context.Context) (runtime.CommittedSessionView, error) {
	if err := ctx.Err(); err != nil {
		return runtime.CommittedSessionView{}, err
	}
	if err := v.pinOperation(false); err != nil {
		return runtime.CommittedSessionView{}, err
	}
	defer v.finishOperation()
	defer func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.committing = false
	}()
	s := v.supervisor
	if err := s.acceptOwner(v.owner, true); err != nil {
		return runtime.CommittedSessionView{}, err
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.viewContext(), cancel)
	defer func() { stop(); cancel() }()
	committed, err := v.prepared.Commit(opCtx)
	if err == nil {
		v.mu.Lock()
		v.committed = true
		v.mu.Unlock()
	}
	return committed, err
}

func (v *hostedView) NewApp(ctx context.Context, committed runtime.CommittedSessionView) (*app.App, error) {
	if err := v.pinOperation(true); err != nil {
		return nil, err
	}
	defer v.finishOperation()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info := v.prepared.Info()
	if committed.Info.SessionID != info.SessionID || committed.Info.RootSessionID != info.RootSessionID {
		return nil, ownerError(info.SessionID, runtime.SessionErrorConflict, "committed view identity differs")
	}
	a, err := v.owner.resources.NewApp(ctx, committed)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	aborted := v.aborted
	v.mu.Unlock()
	v.supervisor.mu.RLock()
	closed := v.supervisor.closed
	v.supervisor.mu.RUnlock()
	if closed || aborted || ctx.Err() != nil {
		if a != nil {
			a.Close()
		}
		return nil, runtime.ErrSessionClosed
	}
	return a, nil
}

// Abort never waits for Commit or NewApp I/O. Their independent operation pins
// drain before owner teardown; runtime Abort retires only unpublished resources.
func (v *hostedView) Abort() {
	v.mu.Lock()
	if v.aborted {
		v.mu.Unlock()
		return
	}
	v.aborted = true
	stop, stopHost, cancel := v.stop, v.stopHost, v.cancel
	v.mu.Unlock()
	if stop != nil {
		stop()
	}
	if stopHost != nil {
		stopHost()
	}
	if cancel != nil {
		cancel()
	}
	v.prepared.Abort()
	s := v.supervisor
	s.mu.Lock()
	delete(s.viewLeases, v)
	// A private factory cleanup can shut down tools or unregister listeners.
	// Keep that work off the UI caller while including it in shutdown drain.
	async := !s.closed
	if async {
		s.ownerOperations.Add(1)
	}
	s.mu.Unlock()
	if async {
		go func() { defer s.ownerOperations.Done(); s.releaseOwner(v.owner) }()
	} else {
		s.releaseOwner(v.owner)
	}
}

// WithSessionOwnerIdentity admits an authorized detached fresh request through
// the same root-key fence without pretending its not-yet-stored row exists.
func (s *Supervisor) WithSessionOwnerIdentity(ctx context.Context, identity ViewOwnerIdentity, use func(context.Context, ViewOwnerResources) error) error {
	if err := s.beginOwnerOperation(); err != nil {
		return err
	}
	defer s.ownerOperations.Done()
	if err := ctx.Err(); err != nil {
		return err
	}
	owner, err := s.acquireOwnerIdentity(ctx, identity.RootSessionID, identity, false)
	if err != nil {
		return err
	}
	defer s.releaseOwner(owner)
	if err := s.acceptOwner(owner, false); err != nil {
		return err
	}
	return use(ctx, owner.resources)
}

// SessionOwnerCleanup is the only normal owned-runtime close callback callers
// retain. Once a view or running child retains the resource, it becomes a no-op;
// only host shutdown can then retire the canonical runtime.
func (s *Supervisor) SessionOwnerCleanup(sessions runtime.SessionRuntime) func() {
	s.mu.RLock()
	var owner *viewOwner
	for _, candidate := range s.ownerResources {
		if !candidate.removed && sameSessionRuntime(candidate.resources.Sessions, sessions) {
			owner = candidate
			break
		}
	}
	s.mu.RUnlock()
	return sync.OnceFunc(func() {
		if owner == nil {
			return
		}
		s.mu.Lock()
		owner.closeRequested = true
		var cleanup func()
		if !owner.removed && !owner.initial && !owner.retained && owner.refs == 0 && !s.ownerHasRunnerLocked(owner) {
			s.removeOwnerLocked(owner)
			cleanup = owner.resources.Cleanup
		}
		s.mu.Unlock()
		if cleanup != nil {
			cleanup()
		}
	})
}

var (
	_ SessionViewAcquirer = (*Supervisor)(nil)
	_ PreparedHostedView  = (*hostedView)(nil)
)
