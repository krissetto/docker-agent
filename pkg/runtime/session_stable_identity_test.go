package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func stableIdentityTeam(rootName, workerName, leafName string, includeIDs bool) *team.Team {
	root := agent.New(rootName, "root instructions", agent.WithModel(coordinationReply("root")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: workerName}))
	worker := agent.New(workerName, "worker instructions", agent.WithModel(coordinationReply("worker")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: leafName}))
	leaf := agent.New(leafName, "leaf instructions", agent.WithModel(coordinationReply("leaf")))
	configs := map[string]latest.AgentConfig{
		rootName: {Name: rootName}, workerName: {Name: workerName}, leafName: {Name: leafName},
	}
	if includeIDs {
		configs[rootName] = latest.AgentConfig{Name: rootName, ID: "root-stable-id"}
		configs[workerName] = latest.AgentConfig{Name: workerName, ID: "worker-stable-id"}
		configs[leafName] = latest.AgentConfig{Name: leafName, ID: "leaf-stable-id"}
	}
	return team.New(team.WithAgents(root, worker, leaf), team.WithAgentConfigs(configs))
}

func stableIdentityRuntime(t *testing.T, store session.Store, tm *team.Team) *LocalRuntime {
	t.Helper()
	r, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	return r
}

func TestNestedSessionRestoreResolvesStableIDsAfterRename(t *testing.T) {
	store := coordinationSQLite(t)
	original := stableIdentityRuntime(t, store, stableIdentityTeam("root", "worker", "leaf", true))
	root, err := original.CreateSession(t.Context(), session.New(session.WithID("stable-root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	child, err := original.CreateSession(t.Context(), session.New(session.WithID("stable-child")), SessionBinding{AgentName: "worker", ParentSessionID: root.ID()})
	require.NoError(t, err)
	grandchild, err := original.CreateSession(t.Context(), session.New(session.WithID("stable-grandchild")), SessionBinding{AgentName: "leaf", ParentSessionID: child.ID()})
	require.NoError(t, err)
	rootSnapshot, err := root.Snapshot(t.Context())
	require.NoError(t, err)
	childSnapshot, err := child.Snapshot(t.Context())
	require.NoError(t, err)
	grandchildSnapshot, err := grandchild.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "root-stable-id", rootSnapshot.AgentID)
	require.Equal(t, "worker-stable-id", childSnapshot.AgentID)
	require.Equal(t, "leaf-stable-id", grandchildSnapshot.AgentID)
	// The authored names remain immutable creation provenance as keys change.
	require.Equal(t, "worker", childSnapshot.AttributesSnapshot()[SessionAgentAttribute])
	require.Equal(t, "root-stable-id", childSnapshot.AttributesSnapshot()[SessionParentAgentIDAttribute])
	require.Equal(t, "worker-stable-id", grandchildSnapshot.AttributesSnapshot()[SessionParentAgentIDAttribute])
	require.NoError(t, original.Close())

	renamed := stableIdentityRuntime(t, store, stableIdentityTeam("root-v2", "worker-v2", "leaf-v2", true))
	prepared, err := (&localSessionRuntimeView{runtime: renamed}).PrepareSessionView(t.Context(), grandchild.ID())
	require.NoError(t, err)
	defer prepared.Abort()
	info := prepared.Info()
	require.Equal(t, "leaf", info.Binding.AgentName, "creation binding stays authored")
	require.Equal(t, "leaf-v2", info.ActiveAgentName, "active execution resolves by stable ID")
	require.NotNil(t, info.Attach)
	require.Equal(t, "leaf-v2", info.Attach.Agent)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	require.Equal(t, "leaf-v2", committed.Info.ActiveAgentName)
	require.Equal(t, "leaf", committed.Info.Binding.AgentName)
	loaded, err := renamed.SessionByID(grandchild.ID())
	require.NoError(t, err)
	require.Equal(t, "leaf-v2", loaded.AgentName())
	snapshot, err := loaded.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "leaf-v2", snapshot.AgentName)
	require.Equal(t, "leaf", snapshot.AttributesSnapshot()[SessionAgentAttribute])
	require.Equal(t, "worker-stable-id", snapshot.AttributesSnapshot()[SessionParentAgentIDAttribute])

	// A second cold process must resolve the same immutable IDs again, rather
	// than depending on aliases materialized by the first restore.
	require.NoError(t, renamed.Close())
	secondResume := stableIdentityRuntime(t, store, stableIdentityTeam("root-v3", "worker-v3", "leaf-v3", true))
	secondPrepared, err := (&localSessionRuntimeView{runtime: secondResume}).PrepareSessionView(t.Context(), grandchild.ID())
	require.NoError(t, err)
	defer secondPrepared.Abort()
	secondInfo := secondPrepared.Info()
	require.Equal(t, "leaf", secondInfo.Binding.AgentName)
	require.Equal(t, "leaf-v3", secondInfo.ActiveAgentName)
	require.Equal(t, "worker-v3", secondInfo.Attach.ParentAgent)
	storedGrandchild, err := store.GetSession(t.Context(), grandchild.ID())
	require.NoError(t, err)
	require.Equal(t, "leaf", storedGrandchild.AttributesSnapshot()[SessionAgentAttribute])
	require.Equal(t, "worker", storedGrandchild.AttributesSnapshot()[SessionParentAgentAttribute])
}

func TestManualSessionIdentityUsesExactLegacyName(t *testing.T) {
	store := session.NewInMemorySessionStore()
	r := stableIdentityRuntime(t, store, stableIdentityTeam("root", "worker", "leaf", false))
	manual := session.New(session.WithID("manual-root"), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	require.NoError(t, store.AddSession(t.Context(), manual))
	loaded, err := r.CreateSession(t.Context(), manual, SessionBinding{})
	require.NoError(t, err)
	require.Equal(t, "root", loaded.AgentName())
	snapshot, err := loaded.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.AgentID, "manual current config without an explicit ID remains name-pinned")
}

func TestRecordedStableIDDoesNotFallbackToReusedName(t *testing.T) {
	store := session.NewInMemorySessionStore()
	oldTeam := stableIdentityTeam("root", "worker", "leaf", true)
	old := stableIdentityRuntime(t, store, oldTeam)
	created, err := old.CreateSession(t.Context(), session.New(session.WithID("reused-name")), SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	snapshot, err := created.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "worker-stable-id", snapshot.AgentID)
	require.NoError(t, old.Close())

	root := agent.New("root", "root", agent.WithModel(coordinationReply("root")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}))
	worker := agent.New("worker", "reused key", agent.WithModel(coordinationReply("new worker")))
	leaf := agent.New("leaf", "leaf", agent.WithModel(coordinationReply("leaf")))
	newTeam := team.New(team.WithAgents(root, worker, leaf), team.WithAgentConfigs(map[string]latest.AgentConfig{
		"root":   {Name: "root", ID: "root-stable-id"},
		"worker": {Name: "worker", ID: "replacement-worker-id"},
		"leaf":   {Name: "leaf", ID: "leaf-stable-id"},
	}))
	restarted := stableIdentityRuntime(t, store, newTeam)
	loaded, err := store.GetSession(t.Context(), "reused-name")
	require.NoError(t, err)
	_, err = restarted.CreateSession(t.Context(), loaded, SessionBinding{})
	require.Error(t, err)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	require.Equal(t, SessionErrorNotFound, sessionErr.Kind)
}

func TestCallerSuppliedAgentIDIsIgnoredOnCreation(t *testing.T) {
	r := stableIdentityRuntime(t, session.NewInMemorySessionStore(), stableIdentityTeam("root", "worker", "leaf", true))
	template := session.New(session.WithID("caller-id"), session.WithAgentName("root"))
	template.AgentID = "forged-id"
	template.SetAttribute(SessionAgentIDAttribute, "forged-creation-id")
	h, err := r.CreateSession(t.Context(), template, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	require.Equal(t, "root-stable-id", snapshot.AgentID)
	require.Equal(t, "root-stable-id", snapshot.AttributesSnapshot()[SessionAgentIDAttribute])
}

func TestManualSessionUsesCurrentSameNameTeamConfig(t *testing.T) {
	store := session.NewInMemorySessionStore()
	oldRoot := agent.New("root", "old prompt", agent.WithModel(coordinationReply("old")))
	oldTeam := team.New(team.WithAgents(oldRoot), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root"}}))
	oldRuntime := stableIdentityRuntime(t, store, oldTeam)
	stored, err := oldRuntime.CreateSession(t.Context(), session.New(session.WithID("manual-current-config")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	oldSnapshot, err := stored.Snapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, oldSnapshot.AgentID)
	require.NoError(t, oldRuntime.Close())

	newRoot := agent.New("root", "new prompt", agent.WithModel(coordinationReply("new")), agent.WithToolSets(&toolListToolset{names: []string{"current_config_tool"}}))
	newTeam := team.New(team.WithAgents(newRoot), team.WithAgentConfigs(map[string]latest.AgentConfig{"root": {Name: "root"}}))
	currentRuntime := stableIdentityRuntime(t, store, newTeam)
	loadedRow, err := store.GetSession(t.Context(), "manual-current-config")
	require.NoError(t, err)
	attached, err := currentRuntime.CreateSession(t.Context(), loadedRow, SessionBinding{})
	require.NoError(t, err)
	require.Equal(t, "root", attached.AgentName())
	info, err := attached.(SessionToolInspector).InspectTools(t.Context())
	require.NoError(t, err)
	var toolNames []string
	for _, tool := range info.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	require.Contains(t, toolNames, "current_config_tool")
	require.Equal(t, "root", loadedRow.AttributesSnapshot()[SessionAgentAttribute])
	require.Empty(t, loadedRow.AttributesSnapshot()[SessionAgentIDAttribute])
}
