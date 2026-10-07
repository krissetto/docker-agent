package root

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	latestcfg "github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	viewhost "github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

type hostViewProvider struct {
	rootTestProvider

	mu      sync.Mutex
	prompts []string
}

func (p *hostViewProvider) CreateChatCompletionStream(_ context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	var prompt strings.Builder
	for _, message := range messages {
		prompt.WriteString(message.Content)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prompts = append(p.prompts, prompt.String())
	return hostViewStream{}, nil
}

func (p *hostViewProvider) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

type hostViewStream struct{}

func (hostViewStream) Recv() (chat.MessageStreamResponse, error) {
	return chat.MessageStreamResponse{}, io.EOF
}
func (hostViewStream) Close() {}

func hostViewLoaded(dir string, model *hostViewProvider) *teamloader.LoadResult {
	agt := agent.New("root", "test instructions", agent.WithModel(model), agent.WithToolSets(filesystem.New(dir)), agent.WithHooks(&hooks.Config{
		TurnStart: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: builtins.AddContext, Args: []string{"HOOK_WORKSPACE={{ .Cwd }}"}}},
	}))
	return &teamloader.LoadResult{Team: team.New(team.WithAgents(agt))}
}

func TestSessionViewHostPrivateResourcesWorkspace(t *testing.T) {
	originalDir, foreignDir := t.TempDir(), t.TempDir()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	store := session.NewInMemorySessionStore()
	flags := &runExecFlags{}
	flags.runConfig.WorkingDir = originalDir
	var resources []*ownedSessionResources
	var models []*hostViewProvider
	for _, dir := range []string{originalDir, foreignDir} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "marker.txt"), []byte(dir), 0o600))
		model := &hostViewProvider{}
		runConfig := flags.runConfig.Clone()
		runConfig.WorkingDir = dir
		resource, err := flags.buildLoadedSessionResources(t.Context(), hostViewLoaded(dir, model), runConfig, store)
		require.NoError(t, err)
		t.Cleanup(resource.cleanup)
		resources = append(resources, resource)
		models = append(models, model)
	}
	flags.listenSessions = newControlPlaneSessions(resources[0].sessions)
	rows, err := store.GetSessions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows, "private construction must not create a blank persisted session")
	assert.Empty(t, flags.listenSessions.extras, "private construction must not register --listen")
	assert.Empty(t, models[0].snapshot())
	assert.Empty(t, models[1].snapshot())

	for _, index := range []int{0, 1, 0, 1} {
		resource := resources[index]
		dir := []string{originalDir, foreignDir}[index]
		defs, err := resource.services.CurrentAgentTools(t.Context())
		require.NoError(t, err)
		var read *tools.Tool
		for i := range defs {
			if defs[i].Name == "read_file" {
				read = &defs[i]
			}
		}
		require.NotNil(t, read)
		result, err := read.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"marker.txt"}`}}, nil)
		require.NoError(t, err)
		assert.Contains(t, result.Output, dir)
		sess := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir))
		handle, err := resource.sessions.CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
		require.NoError(t, err)
		before := len(models[index].snapshot())
		_, err = handle.Submit(t.Context(), runtime.TurnInput{Content: "check hook context"})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return len(models[index].snapshot()) > before }, 5*time.Second, 10*time.Millisecond)
		assert.Contains(t, models[index].snapshot()[before], "HOOK_WORKSPACE="+dir, "canonical turn hooks use the runtime workspace")
	}
	require.NoError(t, resources[1].activate())
	require.NoError(t, resources[1].activate())
	assert.Len(t, flags.listenSessions.extras, 1, "accepted owner is registered once")
	resources[1].cleanup()
	resources[1].cleanup()
	assert.Empty(t, flags.listenSessions.extras)
	require.ErrorIs(t, resources[1].activate(), runtime.ErrSessionClosed)
	assert.Equal(t, originalDir, flags.runConfig.WorkingDir)
	after, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, cwd, after)
}

func TestSessionViewHostSourceScopeAndAncestry(t *testing.T) {
	store := session.NewInMemorySessionStore()
	source := "https://example.invalid/agent?authorization=one"
	scope := viewhost.NewViewOwnerScope()
	root := session.New(session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
	root.SetAttribute(runtime.SessionAgentAttribute, "root")
	root.SetAttribute(sessionActorSourceAttribute, source)
	child := session.New(session.WithAgentName("worker"), session.WithWorkingDir(root.WorkingDir))
	child.ParentID = root.ID
	child.SetAttribute(runtime.SessionAgentAttribute, "worker")
	child.SetAttribute(sessionActorSourceAttribute, source)
	require.NoError(t, store.AddSession(t.Context(), root))
	require.NoError(t, store.AddSession(t.Context(), child))
	resolve := localViewOwnerResolver(scope, source, store)
	identity, err := resolve(t.Context(), child.ID)
	require.NoError(t, err)
	assert.Same(t, scope, identity.Scope)
	assert.Equal(t, root.ID, identity.RootSessionID)
	assert.Equal(t, root.WorkingDir, identity.RootWorkingDir)
	assert.Equal(t, "root", identity.RootBinding.AgentName)
	assert.Empty(t, identity.RootBinding.Model, "mutable model is not owner identity")
	otherScope := viewhost.NewViewOwnerScope()
	other, err := localViewOwnerResolver(otherScope, source, store)(t.Context(), child.ID)
	require.NoError(t, err)
	assert.NotSame(t, identity.Scope, other.Scope)
	_, err = localViewOwnerResolver(scope, "https://example.invalid/agent?authorization=two", store)(t.Context(), child.ID)
	require.Error(t, err, "source query identity must not be stripped")
	_, err = resolve(t.Context(), "missing")
	require.ErrorIs(t, err, session.ErrNotFound)
}

func TestSessionViewHostOrdinaryAndViewShareOwner(t *testing.T) {
	store := session.NewInMemorySessionStore()
	dir := t.TempDir()
	model := &hostViewProvider{}
	registry := provider.NewRegistry(map[string]provider.Factory{
		"test": func(context.Context, *latestcfg.ModelConfig, environment.Provider, ...options.Opt) (provider.Provider, error) {
			return model, nil
		},
	})
	rt, err := runtime.NewLocalRuntime(t.Context(), hostViewLoaded(dir, model).Team, runtime.WithWorkingDir(dir), runtime.WithSessionStore(store), runtime.WithModelSwitcherConfig(&runtime.ModelSwitcherConfig{
		Models:           map[string]latestcfg.ModelConfig{"test/stored": {Provider: "test", Model: "stored"}, "test/live": {Provider: "test", Model: "live"}},
		ProviderRegistry: registry,
		EnvProvider:      environment.NewMapEnvProvider(nil),
	}))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	flags := &runExecFlags{}
	flags.runConfig.WorkingDir = dir
	initial := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir))
	source := config.NewBytesSource("test-source", nil)
	backend := &localBackend{flags: flags, agentSource: source}
	require.NoError(t, flags.configureSessionViewHost(t.Context(), backend, rt, owner.Runtime(), initial, nil, func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }))
	t.Cleanup(flags.sessionViewHost.Shutdown)
	target := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir))
	target.SetAttribute(runtime.SessionAgentAttribute, "root")
	target.AgentModelOverrides = map[string]string{"root": "test/stored"}
	require.NoError(t, store.AddSession(t.Context(), target))
	ordinary, err := flags.restoreHostedSession(t.Context(), target.ID)
	require.NoError(t, err)
	t.Cleanup(ordinary.Close)
	require.NoError(t, ordinary.SessionHandle().SetModel(t.Context(), "test/live"))
	prepared, err := flags.sessionViewHost.AcquireSessionView(t.Context(), target.ID)
	require.NoError(t, err)
	defer prepared.Abort()
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	view, err := prepared.NewApp(t.Context(), committed)
	require.NoError(t, err)
	t.Cleanup(view.Close)
	assert.Same(t, ordinary.SessionRuntime(), view.SessionRuntime())
	assert.Equal(t, target.ID, view.SessionHandle().ID())
	assert.Equal(t, "test/live", view.Binding().Model, "a live mutable model wins over the archived binding")
	assert.Empty(t, model.snapshot(), "ordinary restore and prepared view must not execute")
	view.Close()
	ordinary.Close()
	again, err := flags.restoreHostedSession(t.Context(), target.ID)
	require.NoError(t, err)
	t.Cleanup(again.Close)
	assert.Same(t, view.SessionRuntime(), again.SessionRuntime())
	assert.Equal(t, "test/live", again.Binding().Model)
}

type hostRestoreRuntime struct {
	runtime.SessionRuntime

	restores int
}

func (r *hostRestoreRuntime) PrepareSessionView(ctx context.Context, id string) (runtime.PreparedSessionView, error) {
	r.restores++
	return r.SessionRuntime.(runtime.SessionViewPreparer).PrepareSessionView(ctx, id)
}

func TestSessionViewHostColdRootRestoresTreeOnceAndFreshHTTPUsesInitialWorkspace(t *testing.T) {
	store := session.NewInMemorySessionStore()
	dir := t.TempDir()
	rt, err := runtime.NewLocalRuntime(t.Context(), hostViewLoaded(dir, &hostViewProvider{}).Team, runtime.WithWorkingDir(dir), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	sessions := &hostRestoreRuntime{SessionRuntime: owner.Runtime()}
	flags := &runExecFlags{}
	flags.runConfig.WorkingDir = dir
	initial := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir))
	require.NoError(t, flags.configureSessionViewHost(t.Context(), &localBackend{flags: flags, agentSource: config.NewBytesSource("test", nil)}, rt, sessions, initial, nil, func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }))
	t.Cleanup(flags.sessionViewHost.Shutdown)
	cold := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir))
	cold.SetAttribute(runtime.SessionAgentAttribute, "root")
	require.NoError(t, store.AddSession(t.Context(), cold))
	first, err := flags.restoreHostedSession(t.Context(), cold.ID)
	require.NoError(t, err)
	t.Cleanup(first.Close)
	assert.Equal(t, 1, sessions.restores)
	second, err := flags.restoreHostedSession(t.Context(), cold.ID)
	require.NoError(t, err)
	t.Cleanup(second.Close)
	assert.Equal(t, 2, sessions.restores, "warm reopen still uses confirmed preparation")
	registry := newControlPlaneSessions(sessions, flags)
	fresh := session.New(session.WithAgentName("root"))
	handle, err := registry.CreateSession(t.Context(), fresh, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, dir, snapshot.WorkingDir)
	assert.Empty(t, registry.extras, "empty cwd must use the initial runtime, not build a foreign owner")
}

func TestSessionViewHostRootAndChildRestoreStayDormant(t *testing.T) {
	for _, dormant := range []bool{false, true} {
		name := "ordinary"
		if dormant {
			name = "view"
		}
		t.Run(name, func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			trees := subagent.NewInMemoryStore()
			dir := t.TempDir()
			rootModel, childModel := &hostViewProvider{}, &hostViewProvider{}
			tm := team.New(team.WithAgents(
				agent.New("root", "root", agent.WithModel(rootModel), agent.WithAsyncSubagents(latestcfg.SubagentRef{Agent: "worker"})),
				agent.New("worker", "worker", agent.WithModel(childModel)),
			))
			rt, err := runtime.NewLocalRuntime(t.Context(), tm, runtime.WithWorkingDir(dir), runtime.WithSessionStore(store), runtime.WithSubagentStore(trees), runtime.WithSessionCompaction(false))
			require.NoError(t, err)
			owner := runtime.NewSessionRuntimeSupervisor(rt)
			flags := &runExecFlags{}
			flags.runConfig.WorkingDir = dir
			initial := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir))
			require.NoError(t, flags.configureSessionViewHost(t.Context(), &localBackend{flags: flags, agentSource: config.NewBytesSource("test", nil)}, rt, owner.Runtime(), initial, nil, func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }))
			t.Cleanup(flags.sessionViewHost.Shutdown)
			root := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
			child := session.New(session.WithAgentName("worker"), session.WithParentID(root.ID), session.WithWorkingDir(dir), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "worker", runtime.SessionParentAgentAttribute: "root"}))
			pending := session.UserMessage("pending child work")
			pending.Pending, pending.Accepted, pending.TurnID = true, true, "pending-child"
			child.AddMessage(pending)
			require.NoError(t, store.AddSession(t.Context(), root))
			require.NoError(t, store.AddSession(t.Context(), child))
			rootNode := subagent.SessionRootID(root.ID)
			require.NoError(t, trees.SaveTree(t.Context(), root.ID, subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootNode, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootNode, Agent: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Parent: rootNode, Agent: "worker", SessionID: child.ID, State: subagent.NodeIdle}}}}}}))
			if dormant {
				prepared, err := flags.sessionViewHost.AcquireSessionView(t.Context(), child.ID)
				require.NoError(t, err)
				defer prepared.Abort()
				committed, err := prepared.Commit(t.Context())
				require.NoError(t, err)
				status, err := committed.SessionHandle.Status(t.Context())
				require.NoError(t, err)
				assert.True(t, status.Dormant)
				assert.Equal(t, 1, status.Pending)
				assert.Empty(t, childModel.snapshot())
			} else {
				application, err := flags.restoreHostedSession(t.Context(), root.ID)
				require.NoError(t, err)
				t.Cleanup(application.Close)
				childHandle, err := owner.Runtime().SessionByID(child.ID)
				require.NoError(t, err)
				status, err := childHandle.Status(t.Context())
				require.NoError(t, err)
				assert.True(t, status.Dormant)
				assert.Equal(t, 1, status.Pending)
				assert.Empty(t, childModel.snapshot())
				require.NotNil(t, application.Session().GetSubagentTree())
			}
		})
	}
}

func TestSessionViewHostRemoteNeverBuildsLocal(t *testing.T) {
	store := session.NewInMemorySessionStore()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "test", agent.WithModel(rootTestProvider{})))), runtime.WithSessionStore(store))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	flags := &runExecFlags{remoteAddress: "https://example.invalid/runtime"}
	initial := session.New(session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
	require.NoError(t, flags.configureSessionViewHost(t.Context(), &remoteBackend{flags: flags, agentFileName: "test"}, rt, owner.Runtime(), initial, nil, func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) }))
	t.Cleanup(flags.sessionViewHost.Shutdown)
	foreign := session.New(session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
	foreign.SetAttribute(runtime.SessionAgentAttribute, "root")
	require.NoError(t, store.AddSession(t.Context(), foreign))
	_, err = flags.sessionViewHost.AcquireSessionView(t.Context(), foreign.ID)
	require.Error(t, err, "remote hosts have no local resource factory")
	assert.Nil(t, flags.listenSessions)
}

func TestControlPlaneForwardsPreparedViewCapabilities(t *testing.T) {
	store := session.NewInMemorySessionStore()
	dir := t.TempDir()
	archived := session.New(session.WithID("control-plane-view"), session.WithWorkingDir(dir), session.WithAttributes(map[string]string{runtime.SessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), archived))
	flags := &runExecFlags{}
	flags.runConfig.WorkingDir = dir
	resources, err := flags.buildLoadedSessionResources(t.Context(), hostViewLoaded(dir, &hostViewProvider{}), flags.runConfig.Clone(), store)
	require.NoError(t, err)
	initial := session.New(session.WithAgentName("root"), session.WithWorkingDir(dir))
	backend := &localBackend{flags: flags, agentSource: config.NewBytesSource("test-source", nil)}
	require.NoError(t, flags.configureSessionViewHost(t.Context(), backend, resources.services, resources.sessions, initial, nil, resources.cleanup))
	t.Cleanup(flags.sessionViewHost.Shutdown)
	registry := newControlPlaneSessions(resources.sessions, flags)
	info, err := registry.ConfirmedSessionViewInfo(t.Context(), archived.ID)
	require.NoError(t, err)
	assert.Equal(t, dir, info.WorkingDir)
	_, err = registry.SessionByID(archived.ID)
	require.Error(t, err, "read-only confirmation must not publish")
	prepared, err := registry.PrepareSessionView(t.Context(), archived.ID)
	require.NoError(t, err)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	prepared.Abort()
	status, err := committed.SessionHandle.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.Dormant)
	_, err = registry.SessionByID(archived.ID)
	require.NoError(t, err)
}
