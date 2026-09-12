package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

// factoryRecorder is an SessionRuntimeFactory that records every build and
// hands out real local runtimes, so the routing decisions of
// workspaceSessionRuntimes are observable end to end.
type factoryRecorder struct {
	mu     sync.Mutex
	store  session.Store
	safety latest.SafetyMode
	builds []string // working dirs, in build order
}

func (f *factoryRecorder) build(ctx context.Context, _ config.Source, workingDir string) (runtime.SessionRuntimeSupervisor, error) {
	f.mu.Lock()
	f.builds = append(f.builds, workingDir)
	f.mu.Unlock()
	root := agent.New("root", "prompt", agent.WithModel(sessionHTTPProvider{}), agent.WithSafety(f.safety), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}))
	worker := agent.New("worker", "prompt", agent.WithModel(sessionHTTPProvider{}), agent.WithSafety(f.safety))
	rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(root, worker)), runtime.WithSessionStore(f.store), runtime.WithWorkingDir(workingDir))
	if err != nil {
		return nil, err
	}
	return runtime.NewSessionRuntimeSupervisor(rt), nil
}

func (f *factoryRecorder) built() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.builds...)
}

// memorySource is a config.Source whose bytes tests can swap to simulate a
// refreshed agent definition.
type memorySource struct {
	mu   sync.Mutex
	data string
}

func (s *memorySource) Name() string      { return "agent.yaml" }
func (s *memorySource) ParentDir() string { return "." }
func (s *memorySource) Read(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return []byte(s.data), nil
}

func newFactoryServer(t *testing.T, factory *factoryRecorder, source config.Source, opts ...SessionManagerOpt) (*Server, *SessionManager) {
	t.Helper()
	sm := NewSessionManager(t.Context(), config.Sources{"agent": source}, factory.store, 0, &config.RuntimeConfig{},
		append([]SessionManagerOpt{WithSessionRuntimeFactory(factory.build)}, opts...)...)
	t.Cleanup(func() { require.NoError(t, sm.Shutdown(context.WithoutCancel(t.Context()))) })
	return NewWithManager(sm, ""), sm
}

func createSessionVia(t *testing.T, srv *Server, body string) (sessionMetadataDTO, int) {
	t.Helper()
	rec := sessionRequest(t, srv, http.MethodPost, "/api/sessions", body, "")
	var metadata sessionMetadataDTO
	if rec.Code == http.StatusCreated {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &metadata))
	}
	return metadata, rec.Code
}

// TestSessionRuntimeFactoryRoutesSessionsByWorkingDir pins the per-workspace
// contract of the API: sessions in the server's own directory share one
// runtime, a session with another working_dir gets a runtime rooted there,
// and every session keeps being reachable through the one source registry.
func TestSessionRuntimeFactoryRoutesSessionsByWorkingDir(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	srv, sm := newFactoryServer(t, factory, &memorySource{data: "agents: {}"})
	otherDir := t.TempDir()

	first, code := createSessionVia(t, srv, `{"agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	second, code := createSessionVia(t, srv, `{"agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, []string{""}, factory.built(), "sessions in the default directory share one runtime")

	elsewhere, code := createSessionVia(t, srv, `{"agent_name":"root","working_dir":"`+otherDir+`"}`)
	require.Equal(t, http.StatusCreated, code)
	built := factory.built()
	require.Len(t, built, 2, "another working directory needs its own runtime")
	resolvedOther, _ := filepath.EvalSymlinks(otherDir)
	assert.Equal(t, resolvedOther, built[1])

	stored, err := factory.store.GetSession(t.Context(), elsewhere.SessionID)
	require.NoError(t, err)
	assert.Equal(t, resolvedOther, stored.WorkingDir)

	registry := sm.sessionRegistry
	for _, id := range []string{first.SessionID, second.SessionID, elsewhere.SessionID} {
		_, err := registry.SessionByID(id)
		require.NoError(t, err, "session %s must be reachable through the source registry", id)
	}
	rec := sessionRequest(t, srv, http.MethodGet, "/api/sessions/"+elsewhere.SessionID+"/status", "", "")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestMultiWorkspaceParentPOSTRoutesOwner(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	srv, _ := newFactoryServer(t, factory, &memorySource{data: "agents: {}"})
	other := t.TempDir()
	parent, code := createSessionVia(t, srv, `{"agent_name":"root","working_dir":"`+other+`"}`)
	require.Equal(t, http.StatusCreated, code)
	child, code := createSessionVia(t, srv, `{"agent_name":"worker","parent_session_id":"`+parent.SessionID+`"}`)
	require.Equal(t, http.StatusCreated, code)
	stored, err := factory.store.GetSession(t.Context(), child.SessionID)
	require.NoError(t, err)
	assert.Equal(t, parent.SessionID, stored.ParentID)
	assert.Equal(t, other, stored.WorkingDir)
}

func TestMultiWorkspaceTreeRoutesOwner(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	srv, _ := newFactoryServer(t, factory, &memorySource{data: "agents: {}"})
	other := t.TempDir()
	parent, code := createSessionVia(t, srv, `{"agent_name":"root","working_dir":"`+other+`"}`)
	require.Equal(t, http.StatusCreated, code)
	rec := sessionRequest(t, srv, http.MethodGet, "/api/sessions/"+parent.SessionID+"/tree", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var snapshot subagent.Snapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
	require.Len(t, snapshot.Nodes, 1)
	assert.Equal(t, subagent.SessionRootID(parent.SessionID), snapshot.Root)
}

// --session-workingdir-root meaningful for the session API: a working_dir
// outside the root, or one carrying "..", is a 400 and builds nothing.
func TestSessionRuntimeFactoryRejectsWorkingDirOutsideRoot(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	root := t.TempDir()
	inside := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(inside, 0o755))
	srv, _ := newFactoryServer(t, factory, &memorySource{data: "agents: {}"}, WithSessionWorkingDirRoot(root))

	_, code := createSessionVia(t, srv, `{"agent_name":"root","working_dir":"`+t.TempDir()+`"}`)
	assert.Equal(t, http.StatusBadRequest, code)
	_, code = createSessionVia(t, srv, `{"agent_name":"root","working_dir":"`+inside+`/../project"}`)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Empty(t, factory.built(), "a rejected working_dir must not build a runtime")

	_, code = createSessionVia(t, srv, `{"agent_name":"root","working_dir":"`+inside+`"}`)
	assert.Equal(t, http.StatusCreated, code)
}

// TestSessionRuntimeFactoryRebuildsAfterSourceRefresh pins --pull-interval
// semantics: once the source's configuration changes, the next session runs
// on a runtime built from the new definition while existing sessions keep
// theirs.
func TestSessionRuntimeFactoryRebuildsAfterSourceRefresh(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	source := &memorySource{data: "agents: {v: 1}"}
	srv, sm := newFactoryServer(t, factory, source)

	first, code := createSessionVia(t, srv, `{"agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	require.Len(t, factory.built(), 1)

	loader, ok := sm.Sources["agent"].(*sourceLoader)
	require.True(t, ok)
	before := loader.Generation()
	loader.load(t.Context()) // unchanged bytes: same generation
	assert.Equal(t, before, loader.Generation())
	source.mu.Lock()
	source.data = "agents: {v: 2}"
	source.mu.Unlock()
	loader.load(t.Context())
	assert.Equal(t, before+1, loader.Generation())

	second, code := createSessionVia(t, srv, `{"agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	assert.Len(t, factory.built(), 2, "a refreshed source builds a new runtime for new sessions")

	for _, id := range []string{first.SessionID, second.SessionID} {
		_, err := sm.sessionRegistry.SessionByID(id)
		require.NoError(t, err, "session %s must stay reachable across the refresh", id)
	}
}

func configureWorkspaceRuntimePool(router *workspaceSessionRuntimes, ttl time.Duration, cap int) {
	router.configureIdlePolicy(ttl, cap, nil)
}

func TestWorkspaceRuntimeUnusedRouterStartsNoPruner(t *testing.T) {
	router := newWorkspaceSessionRuntimes(t.Context(), &memorySource{data: "agents: {}"}, (&factoryRecorder{}).build)
	select {
	case <-router.pruneDone:
		t.Fatal("unused router must not start or finish a pruner")
	default:
	}
	require.NoError(t, router.Shutdown(context.WithoutCancel(t.Context())))
	select {
	case <-router.pruneDone:
		t.Fatal("shutdown of an unused router should not synthesize a pruner lifecycle")
	default:
	}
}

func TestWorkspaceRuntimePoolBoundsIdleDirectories(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	router := newWorkspaceSessionRuntimes(t.Context(), &memorySource{data: "agents: {}"}, factory.build)
	configureWorkspaceRuntimePool(router, time.Hour, 3)
	t.Cleanup(func() { require.NoError(t, router.Shutdown(context.WithoutCancel(t.Context()))) })

	for i := range 20 {
		sess := session.New(session.WithID(fmt.Sprintf("session-%d", i)), session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
		_, err := router.CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
		require.NoError(t, err)
		require.NoError(t, router.DeleteSession(t.Context(), sess.ID))
	}

	router.mu.Lock()
	defer router.mu.Unlock()
	assert.LessOrEqual(t, len(router.runtimes), 3, "the cap bounds idle workspaces; active entries are intentionally exempt")
}

type generationSource struct {
	memorySource
	generation atomic.Uint64
}

func (s *generationSource) Generation() uint64 { return s.generation.Load() }

type recordingSupervisor struct {
	runtime.SessionRuntimeSupervisor
	shutdown *atomic.Int32
}

func (s recordingSupervisor) Shutdown(ctx context.Context) error {
	s.shutdown.Add(1)
	return s.SessionRuntimeSupervisor.Shutdown(ctx)
}

func TestWorkspaceRuntimeRetiresOldGenerationAfterDelete(t *testing.T) {
	source := &generationSource{memorySource: memorySource{data: "agents: {}"}}
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	var shutdown atomic.Int32
	build := func(ctx context.Context, source config.Source, dir string) (runtime.SessionRuntimeSupervisor, error) {
		supervisor, err := factory.build(ctx, source, dir)
		if err != nil {
			return nil, err
		}
		return recordingSupervisor{SessionRuntimeSupervisor: supervisor, shutdown: &shutdown}, nil
	}
	router := newWorkspaceSessionRuntimes(t.Context(), source, build)
	configureWorkspaceRuntimePool(router, time.Hour, defaultWorkspaceRuntimeIdleCap)
	t.Cleanup(func() { require.NoError(t, router.Shutdown(context.WithoutCancel(t.Context()))) })

	old := session.New(session.WithID("old"), session.WithAgentName("root"))
	_, err := router.CreateSession(t.Context(), old, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	source.generation.Add(1)
	fresh := session.New(session.WithID("fresh"), session.WithAgentName("root"))
	_, err = router.CreateSession(t.Context(), fresh, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	assert.Zero(t, shutdown.Load(), "the old generation still owns a loaded session")

	require.NoError(t, router.DeleteSession(t.Context(), old.ID))
	assert.Eventually(t, func() bool { return shutdown.Load() == 1 }, time.Second, time.Millisecond)
	_, err = router.SessionByID(fresh.ID)
	require.NoError(t, err, "retiring the old generation must not affect the current one")
}

func TestWorkspaceRuntimeNegativeCapClampsToZero(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	router := newWorkspaceSessionRuntimes(t.Context(), &memorySource{data: "agents: {}"}, factory.build)
	configureWorkspaceRuntimePool(router, time.Hour, -3)
	t.Cleanup(func() { require.NoError(t, router.Shutdown(context.WithoutCancel(t.Context()))) })
	rt, _, release, err := router.acquireRuntime(t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, rt)
	release()
	router.mu.Lock()
	defer router.mu.Unlock()
	require.Empty(t, router.runtimes)
}

func TestWorkspaceRuntimeIdleTTLFiresWithoutRequest(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	var shutdown atomic.Int32
	build := func(ctx context.Context, source config.Source, dir string) (runtime.SessionRuntimeSupervisor, error) {
		supervisor, err := factory.build(ctx, source, dir)
		if err != nil {
			return nil, err
		}
		return recordingSupervisor{SessionRuntimeSupervisor: supervisor, shutdown: &shutdown}, nil
	}
	router := newWorkspaceSessionRuntimes(t.Context(), &memorySource{data: "agents: {}"}, build)
	configureWorkspaceRuntimePool(router, 20*time.Millisecond, defaultWorkspaceRuntimeIdleCap)
	t.Cleanup(func() { require.NoError(t, router.Shutdown(context.WithoutCancel(t.Context()))) })

	rt, _, release, err := router.acquireRuntime(t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, rt)
	release()
	assert.Eventually(t, func() bool { return shutdown.Load() == 1 }, time.Second, 5*time.Millisecond,
		"the timer retires an idle entry without another router request")
}

func TestWorkspaceRuntimeDoesNotInferReleasedOrRestoredOwnership(t *testing.T) {
	source := &generationSource{memorySource: memorySource{data: "agents: {}"}}
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	var shutdown atomic.Int32
	build := func(ctx context.Context, source config.Source, dir string) (runtime.SessionRuntimeSupervisor, error) {
		supervisor, err := factory.build(ctx, source, dir)
		if err != nil {
			return nil, err
		}
		return recordingSupervisor{SessionRuntimeSupervisor: supervisor, shutdown: &shutdown}, nil
	}
	router := newWorkspaceSessionRuntimes(t.Context(), source, build)
	configureWorkspaceRuntimePool(router, 10*time.Millisecond, defaultWorkspaceRuntimeIdleCap)
	t.Cleanup(func() { require.NoError(t, router.Shutdown(context.WithoutCancel(t.Context()))) })

	sess := session.New(session.WithID("borrowed"), session.WithAgentName("root"))
	handle, err := router.CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	require.NoError(t, handle.Release(t.Context()))
	source.generation.Add(1)
	router.prune()
	time.Sleep(30 * time.Millisecond)
	assert.Zero(t, shutdown.Load(), "a missing/released borrowed handle is not proof that restored children are absent")

	require.NoError(t, router.DeleteSession(t.Context(), sess.ID))
	assert.Eventually(t, func() bool { return shutdown.Load() == 1 }, time.Second, 5*time.Millisecond,
		"explicit successful deletion is a safe ownership transition")
}

func TestWorkspaceRuntimeContextCancelRejectsAcquireAndShutsDownOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	var shutdown atomic.Int32
	build := func(ctx context.Context, source config.Source, dir string) (runtime.SessionRuntimeSupervisor, error) {
		supervisor, err := factory.build(ctx, source, dir)
		if err != nil {
			return nil, err
		}
		return recordingSupervisor{SessionRuntimeSupervisor: supervisor, shutdown: &shutdown}, nil
	}
	router := newWorkspaceSessionRuntimes(ctx, &memorySource{data: "agents: {}"}, build)
	rt, _, release, err := router.acquireRuntime(t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, rt)
	release()
	cancel()
	require.Eventually(t, func() bool { return shutdown.Load() == 1 }, time.Second, time.Millisecond)
	_, _, _, err = router.acquireRuntime(t.TempDir())
	require.Error(t, err)
	require.NoError(t, router.Shutdown(t.Context()))
	require.Equal(t, int32(1), shutdown.Load(), "cancellation and explicit shutdown are idempotent")
}

func TestWorkspaceRuntimeBuildCancelShutdownRace(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	started, finish := make(chan struct{}), make(chan struct{})
	var shutdown atomic.Int32
	build := func(ctx context.Context, source config.Source, dir string) (runtime.SessionRuntimeSupervisor, error) {
		close(started)
		<-finish
		supervisor, err := factory.build(context.WithoutCancel(ctx), source, dir)
		if err != nil {
			return nil, err
		}
		return recordingSupervisor{SessionRuntimeSupervisor: supervisor, shutdown: &shutdown}, nil
	}
	router := newWorkspaceSessionRuntimes(ctx, &memorySource{data: "agents: {}"}, build)
	acquireDone := make(chan error)
	go func() { _, _, _, err := router.acquireRuntime(t.TempDir()); acquireDone <- err }()
	<-started
	cancel()
	close(finish)
	require.Error(t, <-acquireDone)
	require.NoError(t, router.Shutdown(t.Context()))
	require.Equal(t, int32(1), shutdown.Load(), "a built runtime rejected after cancellation is shut down exactly once")
}

func TestWorkspaceRuntimeShutdownWaitsForFactoryAndLateDisposal(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	var shutdown atomic.Int32
	build := func(ctx context.Context, source config.Source, dir string) (runtime.SessionRuntimeSupervisor, error) {
		close(started)
		<-finish
		supervisor, err := factory.build(ctx, source, dir)
		if err != nil {
			return nil, err
		}
		return recordingSupervisor{SessionRuntimeSupervisor: supervisor, shutdown: &shutdown}, nil
	}
	router := newWorkspaceSessionRuntimes(t.Context(), &memorySource{data: "agents: {}"}, build)
	acquireDone := make(chan error)
	go func() { _, _, _, err := router.acquireRuntime(t.TempDir()); acquireDone <- err }()
	<-started
	shutdownDone := make(chan error)
	go func() { shutdownDone <- router.Shutdown(t.Context()) }()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before the in-flight factory completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	require.Error(t, <-acquireDone)
	require.NoError(t, <-shutdownDone)
	require.Equal(t, int32(1), shutdown.Load())
}

func TestWorkspaceRuntimeCanceledShutdownWaiterDoesNotPoisonTeardown(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	build := func(ctx context.Context, source config.Source, dir string) (runtime.SessionRuntimeSupervisor, error) {
		close(started)
		<-finish
		return factory.build(ctx, source, dir)
	}
	router := newWorkspaceSessionRuntimes(t.Context(), &memorySource{data: "agents: {}"}, build)
	go func() { _, _, _, _ = router.acquireRuntime(t.TempDir()) }()
	<-started
	waiterCtx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, router.Shutdown(waiterCtx), context.Canceled)
	close(finish)
	require.NoError(t, router.Shutdown(t.Context()), "healthy waiter observes internally completed teardown")
}

func TestWorkspaceRuntimeConcurrentFirstBuildOnce(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	var builds atomic.Int32
	build := func(ctx context.Context, source config.Source, dir string) (runtime.SessionRuntimeSupervisor, error) {
		builds.Add(1)
		time.Sleep(25 * time.Millisecond)
		return factory.build(ctx, source, dir)
	}
	router := newWorkspaceSessionRuntimes(t.Context(), &memorySource{data: "agents: {}"}, build)
	t.Cleanup(func() { require.NoError(t, router.Shutdown(context.WithoutCancel(t.Context()))) })
	dir := t.TempDir()

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess := session.New(session.WithID(fmt.Sprintf("concurrent-%d", i)), session.WithAgentName("root"), session.WithWorkingDir(dir))
			_, err := router.CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), builds.Load())
}

// TestSessionHTTPCreateSeedsAuthorSafetyDefault restores the classic API
// behaviour for author-declared safety: a session created without a client
// choice starts in the agent's YAML mode, and an explicit choice wins.
func TestSessionHTTPCreateSeedsAuthorSafetyDefault(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore(), safety: latest.SafetyModeAutonomous}
	srv, _ := newFactoryServer(t, factory, &memorySource{data: "agents: {}"})

	defaulted, code := createSessionVia(t, srv, `{"agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	stored, err := factory.store.GetSession(t.Context(), defaulted.SessionID)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyAutonomous, stored.GetSafetyPolicy(), "the author default seeds a session with no client choice")

	explicit, code := createSessionVia(t, srv, `{"agent_name":"root","safety_policy":"`+string(session.SafetyPolicyStrict)+`"}`)
	require.Equal(t, http.StatusCreated, code)
	stored, err = factory.store.GetSession(t.Context(), explicit.SessionID)
	require.NoError(t, err)
	assert.Equal(t, session.SafetyPolicyStrict, stored.GetSafetyPolicy(), "a client choice is never overridden by the author default")
}
