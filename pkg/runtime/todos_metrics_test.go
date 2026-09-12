package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	todotool "github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func TestRuntimeTodoStoreSharedRootScopeAndIsolation(t *testing.T) {
	store := session.NewInMemorySessionStore()
	rootA, rootB := session.New(), session.New()
	childA1, childA2 := session.New(), session.New()
	childA1.ParentID, childA2.ParentID = rootA.ID, rootA.ID
	for _, s := range []*session.Session{rootA, rootB, childA1, childA2} {
		require.NoError(t, store.AddSession(t.Context(), s))
	}
	r := &LocalRuntime{subagents: &subagentManager{sessions: map[string]*sessionSubagents{
		rootA.ID:   {topLevel: true, sess: rootA, node: subagent.SessionRootID(rootA.ID)},
		rootB.ID:   {topLevel: true, sess: rootB, node: subagent.SessionRootID(rootB.ID)},
		childA1.ID: {node: "a1"}, childA2.ID: {node: "a2"},
	}, children: map[subagent.NodeID]*childRecord{"a1": {sessionID: childA1.ID, parentSession: rootA.ID}, "a2": {sessionID: childA2.ID, parentSession: rootA.ID}}}}
	adapter := runtimeTodoStore{store: store.(session.TodoStore), scope: r.todoRootSessionID}
	require.NoError(t, adapter.SaveTodos(t.Context(), childA1.ID, []todotool.Todo{{ID: "todo_1"}}))
	got, err := adapter.LoadTodos(t.Context(), childA2.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	other, err := adapter.LoadTodos(t.Context(), rootB.ID)
	require.NoError(t, err)
	require.Empty(t, other)
}

func TestSettledSessionSnapshotUsesComputedCost(t *testing.T) {
	sess := session.New()
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "done", Model: "model", Cost: 1.25}))
	sess.SetTokensAndCost(10, 5, 99)
	d := newSessionDriver(&LocalRuntime{maxReplayEvents: 8, maxReplayBytes: 1 << 20}, sess)

	got, status, interactions, pending, position := d.snapshotLocked()
	require.Equal(t, SessionStateSettled, status.State)
	require.Empty(t, interactions)
	require.Empty(t, pending)
	require.Equal(t, 1, position)
	require.InDelta(t, 1.25, got.Cost, 1e-9)
}

func TestSyntheticRootFollowsDriverLifecycle(t *testing.T) {
	m := newTestSubagentManager(t)
	sess := session.New(session.WithID("root-session"))
	d := m.r.sessionDrivers.Get(sess)
	m.ensureRoot(sess, "root")
	rootID := subagent.SessionRootID(sess.ID)
	node, ok := m.tree.Node(rootID)
	require.True(t, ok)
	require.Equal(t, subagent.NodeIdle, node.State)

	d.mu.Lock()
	started := d.startedCallbacksLocked()
	d.mu.Unlock()
	for _, fn := range started {
		fn()
	}
	node, _ = m.tree.Node(rootID)
	require.Equal(t, subagent.NodeRunning, node.State)

	d.mu.Lock()
	d.lastError = "model failed"
	settled := d.settledCallbacksLocked()
	d.mu.Unlock()
	for _, fn := range settled {
		fn()
	}
	node, _ = m.tree.Node(rootID)
	require.Equal(t, subagent.NodeFailed, node.State)
	require.Equal(t, "model failed", node.Error)
	require.True(t, node.NeedsAttention)

	d.mu.Lock()
	d.lastError = ""
	settled = d.settledCallbacksLocked()
	d.mu.Unlock()
	for _, fn := range settled {
		fn()
	}
	node, _ = m.tree.Node(rootID)
	require.Equal(t, subagent.NodeIdle, node.State)
	require.Empty(t, node.Error)
	require.False(t, node.NeedsAttention)
}

func TestSubagentMetricsOwnCostMonotonicAndRestoredSnapshot(t *testing.T) {
	sess := session.New()
	sess.SetUsage(20, 10)
	sess.SetTokensAndCost(20, 10, 9) // stale compatibility field must not become node cost.
	r := &LocalRuntime{ctx: func() context.Context { return t.Context() }, subagentStore: subagent.NewInMemoryStore()}
	r.subagents = newSubagentManager(r)
	defer r.subagents.Close()
	r.subagents.ensureRoot(sess, "root")
	r.subagents.updateSessionMetrics(sess, 2)
	sess.SetUsage(5, 2) // stale concurrent snapshot must not regress counters.
	r.subagents.updateSessionMetrics(sess, 1)
	node, ok := r.subagents.tree.Node(subagent.SessionRootID(sess.ID))
	require.True(t, ok)
	require.Equal(t, int64(20), node.InputTokens)
	require.Equal(t, int64(10), node.OutputTokens)
	require.Equal(t, int64(3), node.ToolCalls)
	snapshot, err := r.subagentStore.LoadTree(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, node.ToolCalls, snapshot.Nodes[0].Node.ToolCalls)
}
