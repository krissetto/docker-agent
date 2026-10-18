package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func viewEnvironmentTeam(name, id string) *team.Team {
	a := agent.New(name, "", agent.WithModel(coordinationReply("view-environment")))
	return team.New(team.WithAgents(a), team.WithAgentConfigs(map[string]latest.AgentConfig{name: {ID: id}}))
}

func TestColdSessionViewRetainsPreparationEnvironmentThroughReservation(t *testing.T) {
	store := session.NewInMemorySessionStore()
	row := session.New(session.WithID("cold-view-env"), session.WithAgentName("root"))
	row.AgentID = "root-stable"
	row.SetAttribute(SessionAgentAttribute, "root")
	row.SetAttribute(SessionAgentIDAttribute, "root-stable")
	require.NoError(t, store.AddSession(t.Context(), row))

	constructorTeam := viewEnvironmentTeam("constructor", "constructor-id")
	preparedTeam := viewEnvironmentTeam("root", "root-stable")
	initial, err := NewTeamEnvironment("r2", "source", "baseline", "team", preparedTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	environments, err := NewTeamEnvironmentStore(initial)
	require.NoError(t, err)
	rt, err := NewLocalRuntime(t.Context(), constructorTeam, WithSessionStore(store), WithTeamEnvironmentStore(environments), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })

	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), row.ID)
	require.NoError(t, err)
	p := prepared.(*preparedSessionView)
	require.NotNil(t, p.environmentLease)
	require.Equal(t, "r2", p.environmentLease.Environment().RevisionID())

	nextTeam := viewEnvironmentTeam("root", "root-stable")
	next, err := NewTeamEnvironment("r3", "source", "baseline", "team", nextTeam, ModelSwitcherConfig{}, func(context.Context) error { return nil })
	require.NoError(t, err)
	require.NoError(t, environments.Publish("source", "r2", next))

	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	driver := committed.SessionHandle.(*sessionHandle).driver
	require.Equal(t, "r2", driver.teamEnvironmentSnapshot().RevisionID(), "reservation must reuse the exact environment validated during preparation")
	current, err := environments.AcquireCurrent()
	require.NoError(t, err)
	require.Equal(t, "r3", current.Environment().RevisionID())
	require.NoError(t, current.Release(t.Context()))
}
