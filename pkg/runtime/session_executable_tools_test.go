package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type executableNamedToolset struct{ tool tools.Tool }

func (s executableNamedToolset) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{s.tool}, nil
}

func TestSessionExecutableToolsUsePinnedEnvironmentAndRejectRetiredHandlers(t *testing.T) {
	calledOld, calledNext := false, false
	oldToolset := executableNamedToolset{tool: tools.Tool{Name: "pinned-tool", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		calledOld = true
		return tools.ResultSuccess("old"), nil
	}}}
	nextToolset := executableNamedToolset{tool: tools.Tool{Name: "next-tool", Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		calledNext = true
		return tools.ResultSuccess("next"), nil
	}}}
	oldTeam := team.New(team.WithAgents(agent.New("root", "old", agent.WithModel(coordinationReply("old")), agent.WithToolSets(oldToolset))))
	oldEnv, err := NewTeamEnvironment("exec-tools-r1", "source", "baseline", "team", oldTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	store, err := NewTeamEnvironmentStore(oldEnv)
	require.NoError(t, err)
	constructorTeam := team.New(team.WithAgents(agent.New("root", "constructor", agent.WithModel(coordinationReply("constructor")), agent.WithToolSets(nextToolset))))
	r, err := NewLocalRuntime(t.Context(), constructorTeam, WithSessionStore(session.NewInMemorySessionStore()), WithTeamEnvironmentStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(r)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	handle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("exec-tools")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)

	executable, err := handle.(SessionExecutableToolProvider).SessionExecutableTools(t.Context())
	require.NoError(t, err)
	require.Len(t, executable, 1)
	require.Equal(t, "pinned-tool", executable[0].Name)
	inspected, err := handle.(SessionToolInspector).InspectTools(t.Context())
	require.NoError(t, err)
	require.Len(t, inspected.Tools, 1)
	require.Equal(t, "pinned-tool", inspected.Tools[0].Name)
	require.Nil(t, inspected.Tools[0].Handler)

	nextTeam := team.New(team.WithAgents(agent.New("root", "new", agent.WithModel(coordinationReply("new")), agent.WithToolSets(nextToolset))))
	nextEnv, err := NewTeamEnvironment("exec-tools-r2", "source", "baseline", "team", nextTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	require.NoError(t, store.Publish("source", "exec-tools-r1", nextEnv))
	result, err := executable[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: "pinned-tool"}}, tools.NopRuntime{})
	require.NoError(t, err)
	require.Equal(t, "old", result.Output)
	require.True(t, calledOld)
	require.False(t, calledNext, "candidate constructor Team must never receive a pinned session tool call")

	// Retire the only owner of r1; a stale descriptor must refuse invocation
	// instead of retargeting its handler to r2.
	driver := handle.(*sessionHandle).driver
	require.NoError(t, owner.Runtime().DeleteSession(t.Context(), handle.ID()))
	_, err = executable[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: "pinned-tool"}}, tools.NopRuntime{})
	require.ErrorContains(t, err, "no environment owner")
	require.NotContains(t, err.Error(), "constructor")
	require.NotNil(t, driver)
	require.False(t, calledNext)
}
