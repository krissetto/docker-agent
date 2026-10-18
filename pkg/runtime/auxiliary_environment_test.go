package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	todotool "github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func TestCapturedOperationEnvironmentDoesNotDriftAfterPublication(t *testing.T) {
	t.Parallel()
	var oldDisposed, newDisposed atomic.Int32
	oldTeam := team.New(team.WithAgents(agent.New("root", "old-authored-instructions", agent.WithModel(&mockProvider{id: "test/old"}))))
	newTeam := team.New(team.WithAgents(agent.New("root", "new-authored-instructions", agent.WithModel(&mockProvider{id: "test/new"}))))
	oldEnv, err := NewTeamEnvironment("r1", "source", "baseline", "team", oldTeam, ModelSwitcherConfig{}, func(context.Context) error { oldDisposed.Add(1); return nil })
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	rt, err := NewLocalRuntime(t.Context(), oldTeam, WithTeamEnvironmentStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("op-env")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	sess, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	opCtx, release, err := rt.captureEnvironmentForOperation(t.Context(), sess)
	require.NoError(t, err)
	oldOperationTeam := rt.teamForContext(opCtx, sess)
	require.Same(t, oldTeam, oldOperationTeam)

	newEnv, err := NewTeamEnvironment("r2", "source", "baseline", "team", newTeam, ModelSwitcherConfig{}, func(context.Context) error { newDisposed.Add(1); return nil })
	require.NoError(t, err)
	require.NoError(t, store.Publish("source", "r1", newEnv))
	current, err := store.AcquireCurrent()
	require.NoError(t, err)
	assert.Same(t, newTeam, current.Environment().Team(), "new operations see the published environment")
	require.NoError(t, current.Release(t.Context()))
	assert.Zero(t, oldDisposed.Load(), "the captured operation lease keeps the old environment alive")
	assert.Same(t, oldTeam, rt.teamForContext(opCtx, sess), "the existing operation remains pinned")

	release()
	assert.Zero(t, oldDisposed.Load(), "the session retains its pinned revision until retirement")
	require.NoError(t, owner.Runtime().DeleteSession(t.Context(), handle.ID()))
	require.Eventually(t, func() bool { return oldDisposed.Load() == 1 }, time.Second, 5*time.Millisecond)
	require.NoError(t, rt.Close())
	assert.EqualValues(t, 1, newDisposed.Load())
}

func TestCacheBuiltinRequiresCapturedEnvironmentWhenRevisionModeEnabled(t *testing.T) {
	t.Parallel()
	teamBefore := team.New(team.WithAgents(agent.New("root", "before", agent.WithModel(&mockProvider{id: "test/before"}))))
	teamAfter := team.New(team.WithAgents(agent.New("root", "after", agent.WithModel(&mockProvider{id: "test/after"}))))
	oldEnv, err := NewTeamEnvironment("r1", "source", "baseline", "team", teamBefore, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	rt, err := NewLocalRuntime(t.Context(), teamBefore, WithTeamEnvironmentStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	input := &hooks.Input{SessionID: "missing-driver", AgentName: "root", LastUserMessage: "question", StopResponse: "answer"}
	_, err = rt.cacheResponseBuiltin(t.Context(), input, nil)
	require.ErrorContains(t, err, "no captured team environment")
	newEnv, err := NewTeamEnvironment("r2", "source", "baseline", "team", teamAfter, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	require.NoError(t, store.Publish("source", "r1", newEnv))
	cacheCtx := context.WithValue(t.Context(), teamEnvironmentContextKey{}, oldEnv)
	_, err = rt.cacheResponseBuiltin(cacheCtx, input, nil)
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))
	require.NoError(t, rt.Close())
}

func TestSessionOperationCapturePinsOldEnvironmentAcrossPublication(t *testing.T) {
	t.Parallel()
	var oldDisposed, newDisposed atomic.Int32
	oldTeam := team.New(team.WithAgents(agent.New("root", "old", agent.WithModel(&mockProvider{id: "test/old"}))))
	newTeam := team.New(team.WithAgents(agent.New("root", "new", agent.WithModel(&mockProvider{id: "test/new"}))))
	oldEnv, err := NewTeamEnvironment("r1", "source", "base", "team", oldTeam, ModelSwitcherConfig{}, func(context.Context) error { oldDisposed.Add(1); return nil })
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	rt, err := NewLocalRuntime(t.Context(), oldTeam, WithTeamEnvironmentStore(store), WithSessionStore(session.NewInMemorySessionStore()), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("aux-env-op")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	opCtx, release, err := rt.captureEnvironmentForOperation(t.Context(), snapshot)
	require.NoError(t, err)
	newEnv, err := NewTeamEnvironment("r2", "source", "base", "team", newTeam, ModelSwitcherConfig{}, func(context.Context) error { newDisposed.Add(1); return nil })
	require.NoError(t, err)
	require.NoError(t, store.Publish("source", "r1", newEnv))
	assert.Same(t, oldTeam, rt.teamForContext(opCtx, snapshot), "captured operation does not drift after publication")
	current, err := store.AcquireCurrent()
	require.NoError(t, err)
	assert.Same(t, newTeam, current.Environment().Team())
	require.NoError(t, current.Release(t.Context()))
	assert.Zero(t, oldDisposed.Load())
	release()
	assert.Zero(t, oldDisposed.Load(), "the session retains its pinned revision until retirement")
	require.NoError(t, owner.Runtime().DeleteSession(t.Context(), handle.ID()))
	require.Eventually(t, func() bool { return oldDisposed.Load() == 1 }, time.Second, 5*time.Millisecond)
}

func TestCandidateTeamTodoBindingsUseRuntimeSessionStorage(t *testing.T) {
	sharedDefinition := todotool.New(todotool.WithShared(true))
	localDefinition := todotool.New()
	oldTeam := team.New(team.WithAgents(agent.New("root", "old", agent.WithModel(coordinationReply("old")), agent.WithToolSets(sharedDefinition))))
	oldEnvironment, err := NewTeamEnvironment("r1", "source", "baseline", "team", oldTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	store := session.NewInMemorySessionStore()
	environments, err := NewTeamEnvironmentStore(oldEnvironment)
	require.NoError(t, err)
	r, err := NewLocalRuntime(t.Context(), oldTeam, WithSessionStore(store), WithTeamEnvironmentStore(environments), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(r)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	oldHandle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("todo-old")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)

	candidateDefinition := todotool.New(todotool.WithShared(true))
	candidateTeam := team.New(team.WithAgents(agent.New("root", "candidate", agent.WithModel(coordinationReply("candidate")), agent.WithToolSets(candidateDefinition, localDefinition))))
	bindings := r.todoBindingsForTeam(candidateTeam)
	require.Contains(t, bindings, candidateDefinition)
	require.Contains(t, bindings, localDefinition)
	ctx := todotool.WithBindings(httpclient.ContextWithSessionID(t.Context(), oldHandle.ID()), bindings)
	toolsForCandidate, err := candidateTeam.DefaultAgent()
	require.NoError(t, err)
	candidateTools, err := toolsForCandidate.Tools(ctx)
	require.NoError(t, err)
	var created tools.Tool
	for _, candidate := range candidateTools {
		if candidate.Name == todotool.ToolNameCreateTodo {
			created = candidate
			break
		}
	}
	require.NotEmpty(t, created.Name)
	_, err = created.Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: created.Name, Arguments: `{"description":"candidate todo"}`}}, tools.NopRuntime{})
	require.NoError(t, err)
	items, err := store.(session.TodoStore).LoadTodos(t.Context(), oldHandle.ID())
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "candidate todo", items[0].Description)
	originalItems, err := sharedDefinition.Todos(t.Context())
	require.NoError(t, err)
	require.Empty(t, originalItems, "candidate runtime bindings must not mutate reusable team definitions")
}

func TestRevisionScopedHooksUseCapturedAgentConfiguration(t *testing.T) {
	oldAgent := agent.New("root", "old", agent.WithModel(coordinationReply("old")))
	oldTeam := team.New(team.WithAgents(oldAgent))
	oldEnv, err := NewTeamEnvironment("hooks-r1", "source", "baseline", "team", oldTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	capturedAgent := agent.New("root", "candidate", agent.WithModel(coordinationReply("new")), agent.WithAddDate(true))
	capturedTeam := team.New(team.WithAgents(capturedAgent))
	capturedEnv, err := NewTeamEnvironment("hooks-r2", "source", "baseline", "team", capturedTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	require.NoError(t, store.Publish("source", "hooks-r1", capturedEnv))
	r, err := NewLocalRuntime(t.Context(), oldTeam, WithTeamEnvironmentStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	ctx := context.WithValue(t.Context(), teamEnvironmentContextKey{}, capturedEnv)
	exec := r.hooksExecForContext(ctx, capturedAgent)
	require.NotNil(t, exec)
	require.True(t, exec.Has(hooks.EventTurnStart), "candidate-owned hook additions must be visible under the captured environment")
	require.Nil(t, r.hooksExecForContext(t.Context(), capturedAgent), "revision runtimes never fall back to constructor-team hook executors")
}
