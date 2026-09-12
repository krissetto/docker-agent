package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

func TestDirectCreateSessionInitializationFailureCleansGeneratedChildRow(t *testing.T) {
	store := session.NewInMemorySessionStore()
	provider := &mockProvider{id: "test/root", stream: newStreamBuilder().AddContent("ok").Build()}
	rootAgent := agent.New("root", "prompt", agent.WithModel(provider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}))
	worker := agent.New("worker", "prompt", agent.WithModel(provider))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(rootAgent, worker)), WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.shutdownSessions(context.WithoutCancel(t.Context())) })
	parent := session.New(session.WithID("parent"), session.WithAgentName("root"))
	_, err = rt.CreateSession(t.Context(), parent, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	child := session.New(session.WithID("child"))
	_, err = rt.CreateSession(t.Context(), child, SessionBinding{AgentName: "worker", ParentSessionID: parent.ID, Durability: subagent.DurabilityDurable})
	require.Error(t, err)
	_, rowErr := store.GetSession(t.Context(), child.ID)
	require.ErrorIs(t, rowErr, session.ErrNotFound)
	_, lookupErr := rt.SessionByID(child.ID)
	assert.Error(t, lookupErr)
}
