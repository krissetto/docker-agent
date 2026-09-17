package tui

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type hostedRootHandle struct {
	*lifecycleHandle

	snapshot *session.Session
}

func (h *hostedRootHandle) Snapshot(context.Context) (*session.Session, error) {
	return h.snapshot.Clone(), nil
}
func (h *hostedRootHandle) AgentName() string { return h.snapshot.AgentName }
func (h *hostedRootHandle) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: h.id, AgentName: h.AgentName(), Model: h.snapshot.AgentModelOverrides[h.AgentName()]}
}

type hostedRootRuntime struct {
	runtime.SessionRuntime

	handles           map[string]*hostedRootHandle
	creates, restores int
}

func (r *hostedRootRuntime) SessionByID(id string) (runtime.SessionHandle, error) {
	if handle := r.handles[id]; handle != nil {
		return handle, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id}
}

func (r *hostedRootRuntime) CreateSession(_ context.Context, sess *session.Session, _ runtime.SessionBinding) (runtime.SessionHandle, error) {
	r.creates++
	handle := &hostedRootHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, snapshot: sess.Clone()}
	r.handles[sess.ID] = handle
	return handle, nil
}

func (r *hostedRootRuntime) RestoreSessionTree(context.Context, *session.Session) error {
	r.restores++
	return nil
}

func hostedRootFixture(t *testing.T) (*supervisor.Supervisor, *hostedRootRuntime, session.Store) {
	t.Helper()
	owner := supervisor.New(nil)
	t.Cleanup(owner.Shutdown)
	rt := &hostedRootRuntime{handles: make(map[string]*hostedRootHandle)}
	store := session.NewInMemorySessionStore()
	services := storeRuntime{store: store}
	scope := supervisor.NewViewOwnerScope()
	identity := supervisor.ViewOwnerIdentity{Scope: scope, Source: "test", RootSessionID: "persisted-root", RootBinding: runtime.SessionBinding{AgentName: "root"}}
	require.NoError(t, owner.ConfigureSessionViews(t.Context(), supervisor.HostViewConfig{Resolve: func(context.Context, string) (supervisor.ViewOwnerIdentity, error) { return identity, nil }, Factory: func(context.Context, supervisor.ViewOwnerIdentity) (supervisor.ViewOwnerResources, error) {
		panic("initial owner duplicated")
	}, MaxRetainedViewOwners: supervisor.DefaultMaxRetainedViewOwners}))
	require.NoError(t, owner.RegisterSessionOwner(identity, supervisor.ViewOwnerResources{Services: services, Sessions: rt, NewApp: func(ctx context.Context, committed runtime.CommittedSessionView) (*app.App, error) {
		return app.NewResolved(ctx, rt, committed, app.WithRuntimeServices(services))
	}}, true))
	return owner, rt, store
}

func TestHostedOrdinaryColdRootRestoresOnceAndWarmReusesWinningModel(t *testing.T) {
	owner, rt, store := hostedRootFixture(t)
	sess := session.New(session.WithID("persisted-root"), session.WithAgentName("root"))
	sess.AgentModelOverrides = map[string]string{"root": "provider/original"}
	require.NoError(t, store.AddSession(t.Context(), sess))
	firstData, err := loadHostedRoot(t.Context(), owner, sess.ID)
	require.NoError(t, err)
	first, err := firstData.newApp(t.Context(), firstData.committed)
	require.NoError(t, err)
	defer first.Close()
	require.Equal(t, 1, rt.creates)
	require.Equal(t, 1, rt.restores)
	rt.handles[sess.ID].snapshot.AgentModelOverrides["root"] = "provider/winner"
	secondData, err := loadHostedRoot(t.Context(), owner, sess.ID)
	require.NoError(t, err)
	second, err := secondData.newApp(t.Context(), secondData.committed)
	require.NoError(t, err)
	defer second.Close()
	require.Same(t, first.SessionHandle(), second.SessionHandle())
	require.Equal(t, "provider/winner", second.Binding().Model)
	require.Equal(t, 1, rt.creates)
	require.Equal(t, 1, rt.restores, "warm canonical winner never replays the tree")
}

func TestHostedOrdinaryChildNeverCreatesTopLevelOwner(t *testing.T) {
	owner, rt, store := hostedRootFixture(t)
	child := session.New(session.WithID("child"), session.WithAgentName("root"))
	child.ParentID = "persisted-root"
	require.NoError(t, store.AddSession(t.Context(), child))
	_, err := loadHostedRoot(t.Context(), owner, child.ID)
	require.Error(t, err, "fixture lacks prepared child capability; never unsafe root fallback")
	require.Zero(t, rt.creates)
}

func TestPublicNewOrdinaryForeignRestoreUsesDistinctDirectoryOwner(t *testing.T) {
	store := session.NewInMemorySessionStore()
	initialDir, foreignDir := t.TempDir(), t.TempDir()
	initialSession := session.New(session.WithID("initial"), session.WithAgentName("root"), session.WithWorkingDir(initialDir))
	persisted := session.New(session.WithID("foreign"), session.WithAgentName("root"), session.WithWorkingDir(foreignDir))
	require.NoError(t, store.AddSession(t.Context(), persisted))
	initialRuntime := &hostedRootRuntime{handles: make(map[string]*hostedRootHandle)}
	foreignRuntime := &hostedRootRuntime{handles: make(map[string]*hostedRootHandle)}
	services := storeRuntime{store: store}
	initial := app.New(t.Context(), initialRuntime, initialSession, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
	var spawns, cleanups atomic.Int32
	spawner := func(ctx context.Context, directory string) (supervisor.SpawnedSession, error) {
		require.Equal(t, foreignDir, directory, "owner factory receives the real target tool/hook cwd")
		spawns.Add(1)
		blank := session.New(session.WithAgentName("root"), session.WithWorkingDir(directory))
		application := app.New(ctx, foreignRuntime, blank, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
		return supervisor.SpawnedSession{App: application, Session: blank, Ownership: supervisor.RuntimeOwned, Cleanup: func() { cleanups.Add(1) }}, nil
	}
	root := New(t.Context(), spawner, initial, initialDir, func() {}, WithHideSidebar()).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	root.handleWindowResize(120, 40)
	_, cmd := root.handleLoadSession(persisted.ID)
	result := cmd().(hostedLoadResult)
	require.NoError(t, result.err)
	root.finishHostedLoad(result)
	require.Same(t, foreignRuntime, root.application.SessionRuntime())
	require.Equal(t, foreignDir, root.application.Session().WorkingDir)
	require.Equal(t, foreignDir, root.supervisor.ActiveRunner().WorkingDir)
	require.Nil(t, initialRuntime.handles[persisted.ID], "foreign canonical ID is never installed into the initial runtime")
	require.NotNil(t, foreignRuntime.handles[persisted.ID])
	require.EqualValues(t, 1, spawns.Load())
	require.Zero(t, cleanups.Load())
	// Simulate another ordinary browser request originating from the initial
	// runtime after the foreign owner is retained. It must not spawn a loser.
	require.NoError(t, registerCompatibilityRoot(t.Context(), root.supervisor, root.compatResolve, initial, nil, initialDir, persisted.ID))
	reopened, err := loadHostedRoot(t.Context(), root.supervisor, persisted.ID)
	require.NoError(t, err)
	require.Same(t, foreignRuntime.handles[persisted.ID], reopened.committed.SessionHandle)
	require.EqualValues(t, 1, spawns.Load(), "retained foreign root wins before any repeated legacy spawn")
	root.cleanupManagedResources()
	require.EqualValues(t, 1, cleanups.Load(), "accepted owned runtime cleanup occurs once at host shutdown")
}

func TestPublicNewArchivedForeignPaneDoesNotInvokeLegacySpawner(t *testing.T) {
	store := session.NewInMemorySessionStore()
	initialDir, foreignDir := t.TempDir(), t.TempDir()
	initialSession := session.New(session.WithID("initial"), session.WithAgentName("root"), session.WithWorkingDir(initialDir))
	persisted := session.New(session.WithID("foreign"), session.WithAgentName("root"), session.WithWorkingDir(foreignDir))
	require.NoError(t, store.AddSession(t.Context(), persisted))
	rt := &hostedRootRuntime{handles: make(map[string]*hostedRootHandle)}
	application := app.New(t.Context(), rt, initialSession, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(storeRuntime{store: store}))
	var spawns atomic.Int32
	root := New(t.Context(), func(context.Context, string) (supervisor.SpawnedSession, error) {
		spawns.Add(1)
		panic("archived pane invoked ordinary spawner")
	}, application, initialDir, func() {}, WithHideSidebar()).(*appModel)
	t.Cleanup(root.cleanupManagedResources)
	root.handleWindowResize(120, 40)
	cmd := root.beginPaneSource(persisted.ID, initialSession.ID, splitRight)
	prepared := cmd().(paneSourcePreparedMsg)
	require.Error(t, prepared.err, "new foreign archived views require explicit private host resources")
	root.finishPaneSourcePrepared(prepared)
	require.Zero(t, spawns.Load())
	require.Nil(t, rt.handles[persisted.ID])
	require.Equal(t, initialSession.ID, root.paneFocus())
}

func TestHostedLoadRejectsReplacedTargetWithoutTouchingNewOwner(t *testing.T) {
	root := splitTestRoot(t)
	root.legacyPresentationOnly = true
	store := session.NewInMemorySessionStore()
	past := session.New(session.WithID("loaded"), session.WithAgentName("root"))
	require.NoError(t, store.AddSession(t.Context(), past))
	root.application = app.New(t.Context(), nil, root.application.Session(), runtime.SessionBinding{}, app.WithRuntimeServices(storeRuntime{store: store}))
	root.supervisor.GetRunner("profile").App = root.application
	command := root.beginHostedLoad(past.ID, "second", nil)
	result := command().(hostedLoadResult)
	require.NoError(t, result.err)
	replacement := session.New(session.WithID("replacement"), session.WithAgentName("root"))
	replacementApp := app.New(t.Context(), nil, replacement, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root.supervisor.ReplaceRunnerApp(t.Context(), "second", supervisor.SpawnedSession{App: replacementApp, Session: replacement, Ownership: supervisor.RuntimeBorrowed}, "")
	root.finishHostedLoad(result)
	require.Same(t, replacementApp, root.supervisor.GetRunner("second").App)
	require.Equal(t, "profile", root.paneFocus())
}
