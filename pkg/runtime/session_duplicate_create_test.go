package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// Duplicate creation is judged by the immutable creation attribute; an active
// agent that differs from it (after a switch) does not make a matching create conflict.
func TestDuplicateCreateKeepsActiveAgentAfterSwitch(t *testing.T) {
	store := session.NewInMemorySessionStore()
	r := serviceRuntime(t, NewSessionService(), store)
	r.team = team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(coordinationReply("unused"))),
		agent.New("worker", "prompt", agent.WithModel(coordinationReply("unused"))),
	))
	stored := session.New(session.WithID("duplicate-switch"), session.WithAgentName("worker"))
	stored.SetAttribute(SessionAgentAttribute, "root")
	require.NoError(t, store.AddSession(t.Context(), stored))

	template := session.New(session.WithID("duplicate-switch"), session.WithAgentName("root"))
	template.SetAttribute(SessionAgentAttribute, "root")
	handle, err := r.CreateSession(t.Context(), template, SessionBinding{})
	require.NoError(t, err)
	assert.Equal(t, "worker", handle.Metadata().AgentName, "active agent survives a matching duplicate create")
}

// A template whose creation attribute is absent never conflicts with the stored creation.
func TestDuplicateCreateWithoutTemplateCreationAttributeDoesNotConflict(t *testing.T) {
	store := session.NewInMemorySessionStore()
	r := serviceRuntime(t, NewSessionService(), store)
	stored := session.New(session.WithID("duplicate-absent"), session.WithAgentName("root"))
	stored.SetAttribute(SessionAgentAttribute, "root")
	require.NoError(t, store.AddSession(t.Context(), stored))

	template := session.New(session.WithID("duplicate-absent"), session.WithAgentName("root"))
	_, err := r.CreateSession(t.Context(), template, SessionBinding{})
	require.NoError(t, err)
}
