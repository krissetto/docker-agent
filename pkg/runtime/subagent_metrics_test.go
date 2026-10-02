package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestSubagentMetricsPersistOnlyDirtyRootAndSkipNoop(t *testing.T) {
	store := &retryingDurableSubagentStore{}
	r := &LocalRuntime{ctx: func() context.Context { return t.Context() }, subagentStore: store}
	m := newSubagentManager(r)
	defer m.Close()
	a, b := session.New(), session.New()
	m.ensureRoot(a, "a")
	m.ensureRoot(b, "b")
	m.persistSnapshot()
	require.NoError(t, m.persistence().flushNow())
	store.mu.Lock()
	require.Equal(t, 2, store.attempts)
	store.mu.Unlock()
	untouched := b.GetSubagentTree()

	child := session.New()
	childID := subagent.NodeID("child")
	require.NoError(t, m.tree.Add(subagent.Node{ID: childID, Parent: subagent.SessionRootID(a.ID), Agent: "worker"}))
	m.sessions[child.ID] = &sessionSubagents{node: childID}
	child.SetUsage(20, 10)
	m.updateSessionMetrics(child, 2)
	require.NoError(t, m.persistence().flushNow())
	store.mu.Lock()
	require.Equal(t, 3, store.attempts, "unrelated root must not be rewritten")
	require.Equal(t, int64(2), store.saved[a.ID].Nodes[0].Children[0].Node.ToolCalls)
	store.mu.Unlock()
	require.Equal(t, untouched, b.GetSubagentTree(), "unrelated session projection is untouched")

	before, _ := m.tree.Node(childID)
	m.updateSessionMetrics(child, 0)
	require.NoError(t, m.persistence().flushNow())
	after, _ := m.tree.Node(childID)
	require.Equal(t, before, after)
	store.mu.Lock()
	require.Equal(t, 3, store.attempts, "no-op metrics must not enqueue a write")
	store.mu.Unlock()

	// A later change on the parent captures its entire subtree, not another root.
	a.SetUsage(7, 3)
	m.updateSessionMetrics(a, 1)
	require.NoError(t, m.persistence().flushNow())
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Equal(t, 4, store.attempts)
	got := store.saved[a.ID].Nodes[0]
	require.Equal(t, int64(7), got.Node.InputTokens)
	require.Equal(t, int64(20), got.Children[0].Node.InputTokens)
}

func TestSubagentMetricsConcurrentCapturePersistsLatest(t *testing.T) {
	r := &LocalRuntime{ctx: func() context.Context { return t.Context() }, subagentStore: subagent.NewInMemoryStore()}
	m := newSubagentManager(r)
	defer m.Close()
	sess := session.New()
	m.ensureRoot(sess, "root")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				m.updateSessionMetrics(sess, 1)
				m.persistSnapshot()
			}
		})
	}
	wg.Wait()
	got, err := r.subagentStore.LoadTree(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(160), got.Nodes[0].Node.ToolCalls)
}

func TestSubagentDirtySnapshotWriteFailureRemainsPending(t *testing.T) {
	store := &retryingDurableSubagentStore{permanent: fmt.Errorf("offline")}
	r := &LocalRuntime{ctx: func() context.Context { return t.Context() }, subagentStore: store}
	m := newSubagentManager(r)
	defer m.Close()
	sess := session.New()
	m.ensureRoot(sess, "root")
	m.updateSessionMetrics(sess, 1)
	require.Error(t, m.persistence().flushNow())
	store.mu.Lock()
	store.permanent = nil
	store.mu.Unlock()
	// Even though the tree's dirty bit was consumed, the failed value is retained.
	require.NoError(t, m.persistence().flushNow())
	got, err := store.LoadTree(t.Context(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, int64(1), got.Nodes[0].Node.ToolCalls)
}

// The baseline reproduces the original two forest snapshots and all-root
// projection. Both paths run without live subscribers, the common headless case.
func BenchmarkSubagentMetricsRootPersistence(b *testing.B) {
	for _, roots := range []int{1, 100, 1000} {
		for _, baseline := range []bool{true, false} {
			name := "dirty_roots"
			if baseline {
				name = "full_forest_baseline"
			}
			b.Run(fmt.Sprintf("%d/%s", roots, name), func(b *testing.B) {
				r := &LocalRuntime{ctx: func() context.Context { return b.Context() }, subagentStore: subagent.NewInMemoryStore()}
				m := newSubagentManager(r)
				defer m.Close()
				var active *session.Session
				for range roots {
					active = session.New()
					m.ensureRoot(active, "root")
				}
				m.persistSnapshot()
				id := subagent.SessionRootID(active.ID)
				b.ReportAllocs()
				for b.Loop() {
					if !baseline {
						m.updateSessionMetrics(active, 1)
						continue
					}
					input, output := active.Usage()
					cost := active.OwnCost()
					m.metricsMu.Lock()
					_ = m.tree.Update(id, func(n *subagent.Node) {
						n.InputTokens, n.OutputTokens = max(n.InputTokens, input), max(n.OutputTokens, output)
						n.Cost = max(n.Cost, cost)
						n.ToolCalls++
					})
					_ = m.tree.Snapshot() // original unconditional publication snapshot
					full := m.tree.Snapshot()
					full.Durability = r.sessionDurability()
					m.mu.Lock()
					for sessionID, tracked := range m.sessions {
						snap, _ := snapshotForRoot(full, tracked.node)
						tracked.sess.SetSubagentTree(&snap)
						_ = r.subagentStore.SaveTree(b.Context(), sessionID, snap)
					}
					m.mu.Unlock()
					m.metricsMu.Unlock()
				}
			})
		}
	}
}

func TestSubagentRootAndChildMetricsSurviveCanonicalRestart(t *testing.T) {
	rt, store, root := newRestoreFixture(t)
	child := session.New(session.WithID("metrics-child"), session.WithAgentName("planner"), session.WithParentID(root.ID))
	target, err := rt.team.Agent("planner")
	require.NoError(t, err)
	require.NoError(t, rt.subagents.registerIdleChild(root, "root", child, target, subagent.AllowedSubagent{Agent: "planner"}))
	id, ok := rt.subagents.nodeForSession(child.ID)
	require.True(t, ok)
	root.SetTokensAndCost(101, 51, 1.25)
	child.SetTokensAndCost(202, 82, 2.5)
	rt.subagents.updateSessionMetrics(root, 3)
	rt.subagents.updateSessionMetrics(child, 7)
	_, err = rt.subagents.stopChild(root.ID, id)
	require.NoError(t, err)
	rootBefore, _ := rt.subagents.tree.Node(subagent.SessionRootID(root.ID))
	childBefore, _ := rt.subagents.tree.Node(id)
	records, err := store.(session.CoordinationStore).LoadChildren(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Zero(t, records[0].Node.ToolCalls, "metrics live in the persisted projection, not transition records")
	require.NoError(t, rt.subagents.CloseContext(t.Context()))
	rt.sessionDrivers.Close()

	restarted, err := NewLocalRuntime(t.Context(), rt.team, WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(restarted.subagents.Close)
	t.Cleanup(restarted.sessionDrivers.Close)
	loaded, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	snapshot, err := restarted.RestoreSubagentTree(t.Context(), loaded)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	for _, before := range []subagent.Node{rootBefore, childBefore} {
		after, ok := restarted.subagents.tree.Node(before.ID)
		require.True(t, ok)
		require.Equal(t, before.Cost, after.Cost)
		require.Equal(t, before.InputTokens, after.InputTokens)
		require.Equal(t, before.OutputTokens, after.OutputTokens)
		require.Equal(t, before.ToolCalls, after.ToolCalls)
	}
	after, _ := restarted.subagents.tree.Node(id)
	require.Equal(t, subagent.NodeStopped, after.State)
}
