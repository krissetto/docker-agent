package supervisor

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type ownerTestRuntime struct {
	runtime.SessionRuntime

	mu      sync.Mutex
	handles map[string]*ownerTestHandle
	prepare func(context.Context, string) (runtime.PreparedSessionView, error)
}

type ownerTestHandle struct {
	runtime.SessionHandle

	id    string
	model string
}

func (h *ownerTestHandle) ID() string        { return h.id }
func (h *ownerTestHandle) AgentName() string { return "agent" }
func (h *ownerTestHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.id, AgentName: "agent", Model: h.model}
}

func (r *ownerTestRuntime) SessionByID(id string) (runtime.SessionHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h := r.handles[id]; h != nil {
		return h, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound}
}

func (r *ownerTestRuntime) PrepareSessionView(ctx context.Context, id string) (runtime.PreparedSessionView, error) {
	if r.prepare != nil {
		return r.prepare(ctx, id)
	}
	return &ownerTestPrepared{r: r, id: id}, nil
}

type ownerTestPrepared struct {
	r      *ownerTestRuntime
	id     string
	commit func(context.Context) error
	aborts atomic.Int32
}

func (p *ownerTestPrepared) Info() runtime.PreparedSessionViewInfo {
	model := "old"
	if h, err := p.r.SessionByID(p.id); err == nil {
		model = h.Metadata().Model
	}
	sess := session.New(session.WithID(p.id))
	return runtime.PreparedSessionViewInfo{SessionID: p.id, RootSessionID: p.id, Session: sess, Binding: runtime.SessionBinding{AgentName: "agent", Model: model}}
}

func (p *ownerTestPrepared) Commit(ctx context.Context) (runtime.CommittedSessionView, error) {
	if p.commit != nil {
		if err := p.commit(ctx); err != nil {
			return runtime.CommittedSessionView{}, err
		}
	}
	p.r.mu.Lock()
	if p.r.handles == nil {
		p.r.handles = make(map[string]*ownerTestHandle)
	}
	h := p.r.handles[p.id]
	if h == nil {
		h = &ownerTestHandle{id: p.id, model: "old"}
		p.r.handles[p.id] = h
	}
	p.r.mu.Unlock()
	return runtime.CommittedSessionView{SessionHandle: h, Info: p.Info()}, nil
}
func (p *ownerTestPrepared) Abort() { p.aborts.Add(1) }

func ownerTestResources(r *ownerTestRuntime, cleanup func()) ViewOwnerResources {
	return ViewOwnerResources{Sessions: r, Cleanup: cleanup, NewApp: func(ctx context.Context, committed runtime.CommittedSessionView) (*app.App, error) {
		return app.NewResolved(ctx, r, committed)
	}}
}

func ownerTestHost(t *testing.T, limit int, factory func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error)) (*Supervisor, *ViewOwnerScope) {
	t.Helper()
	s := New(nil)
	scope := NewViewOwnerScope()
	require.NoError(t, s.ConfigureSessionViews(t.Context(), HostViewConfig{MaxRetainedViewOwners: limit, Factory: factory, Resolve: func(_ context.Context, id string) (ViewOwnerIdentity, error) {
		return ViewOwnerIdentity{Scope: scope, Source: "source?auth=exact", RootSessionID: id, RootWorkingDir: "/foreign"}, nil
	}}))
	t.Cleanup(s.Shutdown)
	return s, scope
}

func TestViewOwnerRetainsBeforeUnknownCommitAndReusesMutableModel(t *testing.T) {
	var builds, cleanups atomic.Int32
	r := &ownerTestRuntime{}
	s, _ := ownerTestHost(t, 1, func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error) {
		builds.Add(1)
		return ownerTestResources(r, func() { cleanups.Add(1) }), nil
	})
	r.prepare = func(context.Context, string) (runtime.PreparedSessionView, error) {
		return &ownerTestPrepared{r: r, id: "root", commit: func(context.Context) error { return errors.New("unknown publication") }}, nil
	}
	view, err := s.AcquireSessionView(t.Context(), "root")
	require.NoError(t, err)
	_, err = view.Commit(t.Context())
	require.ErrorContains(t, err, "unknown publication")
	view.Abort()
	require.Zero(t, cleanups.Load())
	r.prepare = nil
	r.mu.Lock()
	r.handles = map[string]*ownerTestHandle{"root": {id: "root", model: "changed"}}
	r.mu.Unlock()
	view, err = s.AcquireSessionView(t.Context(), "root")
	require.NoError(t, err)
	require.Equal(t, "changed", view.Info().Binding.Model)
	view.Abort()
	require.Equal(t, int32(1), builds.Load())
	require.Zero(t, s.Count())
	s.Shutdown()
	require.Equal(t, int32(1), cleanups.Load())
}

func TestViewOwnerCapacityReservationBeforeFactoryAndZero(t *testing.T) {
	for _, limit := range []int{0, DefaultMaxRetainedViewOwners} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			var builds, cleanups atomic.Int32
			s, _ := ownerTestHost(t, limit, func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error) {
				builds.Add(1)
				return ownerTestResources(&ownerTestRuntime{}, func() { cleanups.Add(1) }), nil
			})
			for i := range limit {
				view, err := s.AcquireSessionView(t.Context(), strconv.Itoa(i))
				require.NoError(t, err)
				_, err = view.Commit(t.Context())
				require.NoError(t, err)
				view.Abort()
			}
			_, err := s.AcquireSessionView(t.Context(), "overflow")
			require.ErrorIs(t, err, runtime.ErrSessionCapacity)
			require.Equal(t, int32(limit), builds.Load())
			require.Zero(t, cleanups.Load())
			s.Shutdown()
			require.Equal(t, int32(limit), cleanups.Load())
		})
	}
}

func TestViewOwnerConcurrentAcquisitionOneFactoryAndPrecommitCleanup(t *testing.T) {
	var builds, cleanups atomic.Int32
	entered, resume := make(chan struct{}), make(chan struct{})
	s, _ := ownerTestHost(t, 1, func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error) {
		builds.Add(1)
		close(entered)
		<-resume
		return ownerTestResources(&ownerTestRuntime{}, func() { cleanups.Add(1) }), nil
	})
	views := make(chan PreparedHostedView, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() { v, err := s.AcquireSessionView(t.Context(), "root"); views <- v; errs <- err }()
	}
	<-entered
	close(resume)
	first, second := <-views, <-views
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	first.Abort()
	require.Zero(t, cleanups.Load())
	second.Abort()
	require.Equal(t, int32(1), builds.Load())
	require.Eventually(t, func() bool { return cleanups.Load() == 1 }, time.Second, time.Millisecond)
}

func TestViewOwnerInitialAliasesExcludedAndSafetyRetentionAtCapacity(t *testing.T) {
	var initialCleanup, ordinaryCleanup atomic.Int32
	s, scope := ownerTestHost(t, 0, func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error) {
		t.Fatal("factory must not bypass existing owner")
		return ViewOwnerResources{}, nil
	})
	initial := &ownerTestRuntime{handles: map[string]*ownerTestHandle{"child": {id: "child"}}}
	identity := ViewOwnerIdentity{Scope: scope, Source: "source?auth=exact", RootSessionID: "root", RootWorkingDir: "/foreign"}
	resources := ownerTestResources(initial, func() { initialCleanup.Add(1) })
	require.NoError(t, s.RegisterSessionOwner(identity, resources, true))
	identity.RootSessionID = "alias"
	require.NoError(t, s.RegisterSessionOwner(identity, resources, true))
	view, err := s.AcquireSessionView(t.Context(), "child")
	require.NoError(t, err)
	_, err = view.Commit(t.Context())
	require.NoError(t, err)
	view.Abort()
	require.Len(t, s.ownerResources, 1)
	ordinary := &ownerTestRuntime{}
	identity.RootSessionID = "ordinary"
	require.NoError(t, s.RegisterSessionOwner(identity, ownerTestResources(ordinary, func() { ordinaryCleanup.Add(1) }), false))
	// Simulate the existing runner's structural resource association without
	// starting real App services or tools.
	_, err = s.AddSession(t.Context(), nil, session.New(session.WithID("ordinary")), "", nil)
	require.NoError(t, err)
	s.mu.Lock()
	s.runners["ordinary"].owner = s.viewOwners[identity.key()]
	s.mu.Unlock()
	s.RetainCleanupUntilShutdown("ordinary")
	s.CloseSession("ordinary")
	s.SessionOwnerCleanup(ordinary)()
	require.Zero(t, ordinaryCleanup.Load())
	s.Shutdown()
	require.Equal(t, int32(1), ordinaryCleanup.Load())
	require.Equal(t, int32(1), initialCleanup.Load())
}

func TestViewOwnerNormalCloseInvalidatesResourceAndScopesDoNotAlias(t *testing.T) {
	var cleanups atomic.Int32
	s, scope := ownerTestHost(t, 1, nil)
	r := &ownerTestRuntime{}
	identity := ViewOwnerIdentity{Scope: scope, Source: "source?auth=exact", RootSessionID: "root"}
	require.NoError(t, s.RegisterSessionOwner(identity, ownerTestResources(r, func() { cleanups.Add(1) }), false))
	other := identity
	other.Scope = NewViewOwnerScope()
	require.Error(t, s.RegisterSessionOwner(other, ownerTestResources(r, nil), false))
	other = identity
	other.Source = "source?auth=other"
	require.Error(t, s.RegisterSessionOwner(other, ownerTestResources(r, nil), false))
	closeOwner := s.SessionOwnerCleanup(r)
	closeOwner()
	closeOwner()
	require.Equal(t, int32(1), cleanups.Load())
	require.Empty(t, s.ownerResources)
	_, err := s.AcquireSessionView(t.Context(), "root")
	require.Error(t, err)
	s.Shutdown()
	require.Equal(t, int32(1), cleanups.Load())
}

func TestViewOwnerShutdownCancelsFactoryAndCommitOutsideLock(t *testing.T) {
	for _, stage := range []string{"factory", "commit"} {
		t.Run(stage, func(t *testing.T) {
			entered := make(chan struct{})
			var cleanup atomic.Int32
			r := &ownerTestRuntime{}
			s, _ := ownerTestHost(t, 1, func(ctx context.Context, _ ViewOwnerIdentity) (ViewOwnerResources, error) {
				if stage == "factory" {
					close(entered)
					<-ctx.Done()
				}
				return ownerTestResources(r, func() { cleanup.Add(1) }), nil
			})
			if stage == "commit" {
				r.prepare = func(context.Context, string) (runtime.PreparedSessionView, error) {
					return &ownerTestPrepared{r: r, id: "root", commit: func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }}, nil
				}
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				view, err := s.AcquireSessionView(t.Context(), "root")
				if err == nil {
					_, _ = view.Commit(t.Context())
					view.Abort()
				}
			}()
			<-entered
			s.Shutdown()
			<-done
			require.Equal(t, int32(1), cleanup.Load())
			_, err := s.AcquireSessionView(t.Context(), "root")
			require.ErrorIs(t, err, runtime.ErrSessionClosed)
		})
	}
}

func TestViewOwnerShutdownRejectsNewAppAndAdoption(t *testing.T) {
	s, _ := ownerTestHost(t, 1, func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error) {
		return ownerTestResources(&ownerTestRuntime{}, nil), nil
	})
	view, err := s.AcquireSessionView(t.Context(), "root")
	require.NoError(t, err)
	committed, err := view.Commit(t.Context())
	require.NoError(t, err)
	s.Shutdown()
	_, err = view.NewApp(t.Context(), committed)
	require.ErrorIs(t, err, runtime.ErrSessionClosed)
	_, err = s.AddSession(t.Context(), nil, session.New(session.WithID("late")), "", nil)
	require.ErrorIs(t, err, runtime.ErrSessionClosed)
}

func TestViewOwnerAbortDoesNotWaitForCommit(t *testing.T) {
	entered, resume := make(chan struct{}), make(chan struct{})
	var cleanup atomic.Int32
	r := &ownerTestRuntime{}
	r.prepare = func(context.Context, string) (runtime.PreparedSessionView, error) {
		return &ownerTestPrepared{r: r, id: "root", commit: func(context.Context) error { close(entered); <-resume; return nil }}, nil
	}
	s, _ := ownerTestHost(t, 1, func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error) {
		return ownerTestResources(r, func() { cleanup.Add(1) }), nil
	})
	view, err := s.AcquireSessionView(t.Context(), "root")
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = view.Commit(t.Context()) }()
	<-entered
	view.Abort()
	// The gate remains closed until Abort has synchronously returned. A view
	// cancellation cannot wait for runtime SQL or revoke its retained owner.
	require.Zero(t, cleanup.Load())
	close(resume)
	<-done
	s.Shutdown()
	require.Equal(t, int32(1), cleanup.Load())
}

func TestViewOwnerOrdinaryAdmissionAndListenCleanupShareOwner(t *testing.T) {
	var builds, registrations, cleanup atomic.Int32
	var s *Supervisor
	s, _ = ownerTestHost(t, 0, func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error) {
		builds.Add(1)
		resources := ownerTestResources(&ownerTestRuntime{}, func() {
			require.Zero(t, s.Count())
			cleanup.Add(1)
		})
		resources.Retain = func() error {
			// --listen registration may call back into the host, never under mu.
			require.Zero(t, s.Count())
			registrations.Add(1)
			return nil
		}
		return resources, nil
	})
	require.NoError(t, s.WithSessionOwner(t.Context(), "root", func(_ context.Context, resources ViewOwnerResources) error {
		r := resources.Sessions.(*ownerTestRuntime)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.handles = map[string]*ownerTestHandle{"root": {id: "root"}}
		return nil
	}))
	view, err := s.AcquireSessionView(t.Context(), "root")
	require.NoError(t, err, "ordinary safety owner reuse needs no optional capacity")
	_, err = view.Commit(t.Context())
	require.NoError(t, err)
	view.Abort()
	require.Equal(t, int32(1), builds.Load())
	require.Equal(t, int32(1), registrations.Load())
	s.Shutdown()
	require.Equal(t, int32(1), cleanup.Load())
}

type ownerReplacementServices struct{ app.Services }

func (ownerReplacementServices) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {
}
func (ownerReplacementServices) OnToolsChanged(func(runtime.Event))    {}
func (ownerReplacementServices) OnBackgroundEvent(func(runtime.Event)) {}

func TestViewOwnerReplacementTransfersInitialBorrowedCleanup(t *testing.T) {
	var cleanup atomic.Int32
	s, scope := ownerTestHost(t, 0, nil)
	r := &ownerTestRuntime{}
	identity := ViewOwnerIdentity{Scope: scope, Source: "source?auth=exact", RootSessionID: "root"}
	require.NoError(t, s.RegisterSessionOwner(identity, ownerTestResources(r, nil), true))
	sess := session.New(session.WithID("root"))
	_, err := s.AddSession(t.Context(), nil, sess, "", nil)
	require.NoError(t, err)
	// A nil session avoids an execution handle entirely: this regression tests
	// resource transfer, not driver startup or observation ownership.
	application := app.New(t.Context(), r, nil, runtime.SessionBinding{}, app.WithRuntimeServices(ownerReplacementServices{}))
	s.ReplaceRunnerApp(t.Context(), sess.ID, SpawnedSession{
		App: application, Session: sess, Ownership: RuntimeOwned,
		Cleanup: func() { cleanup.Add(1) },
	}, "")
	s.RetainCleanupUntilShutdown(sess.ID)
	s.CloseSession(sess.ID)
	require.Zero(t, cleanup.Load())
	s.Shutdown()
	s.Shutdown()
	require.Equal(t, int32(1), cleanup.Load())
}

func TestViewOwnerAddTransfersInitialBorrowedCleanup(t *testing.T) {
	var cleanup atomic.Int32
	s, scope := ownerTestHost(t, 0, nil)
	r := &ownerTestRuntime{}
	identity := ViewOwnerIdentity{Scope: scope, Source: "source?auth=exact", RootSessionID: "root"}
	require.NoError(t, s.RegisterSessionOwner(identity, ownerTestResources(r, nil), true))
	application := app.New(t.Context(), r, nil, runtime.SessionBinding{}, app.WithRuntimeServices(ownerReplacementServices{}))
	_, err := s.AddSession(t.Context(), application, session.New(session.WithID("root")), "", func() { cleanup.Add(1) })
	require.NoError(t, err)
	s.RetainCleanupUntilShutdown("root")
	s.CloseSession("root")
	require.Zero(t, cleanup.Load())
	s.Shutdown()
	require.Equal(t, int32(1), cleanup.Load())
}

func TestViewOwnerReplacementKeepsExistingCleanupAuthority(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(strconv.FormatBool(retain), func(t *testing.T) {
			var cleanup atomic.Int32
			s, scope := ownerTestHost(t, 0, nil)
			r := &ownerTestRuntime{}
			identity := ViewOwnerIdentity{Scope: scope, Source: "source?auth=exact", RootSessionID: "root"}
			require.NoError(t, s.RegisterSessionOwner(identity, ownerTestResources(r, func() { cleanup.Add(1) }), false))
			sess := session.New(session.WithID("root"))
			_, err := s.AddSession(t.Context(), nil, sess, "", nil)
			require.NoError(t, err)
			application := app.New(t.Context(), r, nil, runtime.SessionBinding{}, app.WithRuntimeServices(ownerReplacementServices{}))
			closeOwner := s.SessionOwnerCleanup(r)
			s.ReplaceRunnerApp(t.Context(), sess.ID, SpawnedSession{
				App: application, Session: sess, Ownership: RuntimeOwned, Cleanup: closeOwner,
			}, "")
			if retain {
				s.RetainCleanupUntilShutdown(sess.ID)
			}
			s.CloseSession(sess.ID)
			closeOwner()
			if retain {
				require.Zero(t, cleanup.Load())
			} else {
				require.Eventually(t, func() bool { return cleanup.Load() == 1 }, time.Second, time.Millisecond)
			}
			s.Shutdown()
			closeOwner()
			require.Equal(t, int32(1), cleanup.Load())
		})
	}
}

type ownerInfoRuntime struct {
	*ownerTestRuntime

	read func(context.Context, string) (runtime.PreparedSessionViewInfo, error)
}

func (r *ownerInfoRuntime) ConfirmedSessionViewInfo(ctx context.Context, id string) (runtime.PreparedSessionViewInfo, error) {
	return r.read(ctx, id)
}

func TestConfirmedSessionViewInfoReleasesTemporaryOwner(t *testing.T) {
	var builds, cleanups atomic.Int32
	r := &ownerInfoRuntime{ownerTestRuntime: &ownerTestRuntime{}, read: func(_ context.Context, id string) (runtime.PreparedSessionViewInfo, error) {
		return runtime.PreparedSessionViewInfo{SessionID: id, RootSessionID: id}, nil
	}}
	s, _ := ownerTestHost(t, 1, func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error) {
		builds.Add(1)
		resources := ownerTestResources(r.ownerTestRuntime, func() { cleanups.Add(1) })
		resources.Sessions = r
		return resources, nil
	})
	for _, id := range []string{"first", "second"} {
		info, err := s.ConfirmedSessionViewInfo(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, id, info.SessionID)
	}
	require.EqualValues(t, 2, builds.Load(), "metadata reads cannot consume retained-owner capacity")
	require.EqualValues(t, 2, cleanups.Load())
	require.Zero(t, s.Count())
	s.Shutdown()
	require.EqualValues(t, 2, cleanups.Load(), "temporary ownership released exactly once")
}

func TestConfirmedSessionViewInfoHostShutdownCancelsRead(t *testing.T) {
	entered := make(chan struct{})
	var cleanups atomic.Int32
	r := &ownerInfoRuntime{ownerTestRuntime: &ownerTestRuntime{}, read: func(ctx context.Context, _ string) (runtime.PreparedSessionViewInfo, error) {
		close(entered)
		<-ctx.Done()
		return runtime.PreparedSessionViewInfo{}, ctx.Err()
	}}
	s, _ := ownerTestHost(t, 1, func(context.Context, ViewOwnerIdentity) (ViewOwnerResources, error) {
		resources := ownerTestResources(r.ownerTestRuntime, func() { cleanups.Add(1) })
		resources.Sessions = r
		return resources, nil
	})
	result := make(chan error, 1)
	go func() { _, err := s.ConfirmedSessionViewInfo(t.Context(), "root"); result <- err }()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("read failed before admission: %v", err)
	case <-time.After(time.Second):
		t.Fatal("read did not enter")
	}
	s.Shutdown()
	require.ErrorIs(t, <-result, context.Canceled)
	require.EqualValues(t, 1, cleanups.Load())
}
