package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

func lifecycleTeam() *team.Team {
	provider := &mockProvider{id: "test/lifecycle", stream: newStreamBuilder().AddContent("ok").Build()}
	return team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(provider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(provider)),
	))
}

func TestParentCreatedChildSQLiteShutdownRestoreLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	permissions := &session.PermissionsConfig{Allow: []string{"read"}}
	root := session.New(session.WithID("root"), session.WithAgentName("root"), session.WithSafetyPolicy(session.SafetyPolicyStrict), session.WithPermissions(permissions), session.WithToolsApproved(true))
	rt, err := NewLocalRuntime(t.Context(), lifecycleTeam(), WithSessionStore(store))
	require.NoError(t, err)
	view := NewSessionRuntimeSupervisor(rt).Runtime()
	_, err = view.CreateSession(t.Context(), root, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	child := session.New(session.WithID("child"), session.WithTitle("client child"))
	_, err = view.CreateSession(t.Context(), child, SessionBinding{AgentName: "worker", ParentSessionID: root.ID})
	require.NoError(t, err)
	stored, err := store.GetSession(t.Context(), child.ID)
	require.NoError(t, err)
	assert.Equal(t, root.ID, stored.ParentID)
	inheritedTools, inheritedSafety, inheritedPermissions := root.SafetySettings()
	assert.Equal(t, inheritedSafety, stored.GetSafetyPolicy())
	assert.Equal(t, inheritedTools, stored.ToolsApproved)
	assert.Equal(t, inheritedPermissions, stored.ClonePermissions())
	assert.Equal(t, "worker", stored.AttributesSnapshot()[SessionAgentAttribute])
	treeBefore, err := view.(TreeInspector).InspectSessionTree(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, treeBefore.Nodes[0].Children, 1)
	require.NoError(t, rt.shutdownSessions(context.WithoutCancel(t.Context())))
	require.NoError(t, store.(*session.SQLiteSessionStore).Close())

	store2, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	defer store2.(*session.SQLiteSessionStore).Close()
	rt2, err := NewLocalRuntime(t.Context(), lifecycleTeam(), WithSessionStore(store2))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt2.shutdownSessions(context.WithoutCancel(t.Context()))) }()
	loadedRoot, err := store2.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	_, err = rt2.CreateSession(t.Context(), loadedRoot, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	_, err = rt2.RestoreSubagentTree(t.Context(), loadedRoot)
	require.NoError(t, err)
	attached, err := rt2.SessionByID(child.ID)
	require.NoError(t, err)
	assert.Equal(t, child.ID, attached.ID())
	restored, err := (&localSessionRuntimeView{runtime: rt2}).InspectSessionTree(t.Context(), child.ID)
	require.NoError(t, err)
	assert.Equal(t, treeBefore.Nodes[0].Children[0].Node.ID, restored.Root)
}
