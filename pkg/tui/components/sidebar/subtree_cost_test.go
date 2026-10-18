package sidebar

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func costTree() subagent.Snapshot {
	return subagent.Snapshot{Root: "root:root", Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: "root:root", Agent: "root", State: subagent.NodeIdle, Cost: 1},
		Children: []subagent.NodeSnapshot{{
			Node:     subagent.Node{ID: "child-node", SessionID: "child", Agent: "worker", State: subagent.NodeCompleted, Cost: 2},
			Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "grand-node", SessionID: "grand", Agent: "worker", State: subagent.NodeStopped, Cost: 3}}},
		}, {Node: subagent.Node{ID: "sibling-node", SessionID: "sibling", Agent: "worker", State: subagent.NodeFailed, Cost: 4}}},
	}, {Node: subagent.Node{ID: "root:other", Agent: "root", Cost: 100}}}}
}

func TestSubtreeCostCanonicalAttachedScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id, node string
		want     float64
	}{
		{"root", "", 10},
		{"child", "child-node", 5},
		{"grand", "grand-node", 3},
		{"solo", "", 0},
	} {
		t.Run(tc.id, func(t *testing.T) {
			m := newTestSidebar(t)
			if tc.node != "" {
				m.SetSubagentContext(subagent.NodeID(tc.node), "parent", "parent-session")
			}
			m.LoadFromSession(session.New(session.WithID(tc.id)))
			m.SetSubagentTree(costTree())
			stats := m.computeUsageStats()
			assert.InDelta(t, tc.want, stats.totalCost, 1e-9)
			assert.Zero(t, stats.sessionCount, "tree costs do not alter observed usage counts")
		})
	}
}

func TestSubtreeCostIdleRootLiveUsageAndReplay(t *testing.T) {
	t.Parallel()
	m := newTestSidebar(t)
	root := session.New(session.WithID("root"))
	root.SetUsage(40, 2)
	m.LoadFromSession(root)
	tree := costTree()
	m.SetSubagentTree(tree)
	for _, cost := range []float64{5, 5, 3} {
		m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "grand", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{Cost: cost, OwnCost: &cost, InputTokens: 999}})
	}
	// An old/duplicate tree publication, plus a sibling attachment's unrelated
	// accounting, must not add spend or regress a cumulative descendant snapshot.
	m.SetSubagentTree(tree)
	foreignCost := 200.0
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "other", AgentContext: runtime.AgentContext{AgentName: "root"}, Usage: &runtime.Usage{Cost: foreignCost, OwnCost: &foreignCost}})
	assert.InDelta(t, 12, m.SubtreeCost(), 1e-9)
	assert.Equal(t, int64(42), m.computeUsageStats().tokens, "idle root tokens remain root-only")
	assert.Contains(t, ansi.Strip(m.tokenUsageSummary()), "$12.00")
	assert.Contains(t, ansi.Strip(m.tokenUsage(60)), "$12.00")
}

func TestSubtreeCostRestoreUnionAndLegacyAggregate(t *testing.T) {
	t.Parallel()
	root := session.New(session.WithID("root"))
	root.Messages = append(root.Messages, sidebarCostItem("root", 1))
	child := session.New(session.WithID("child"), session.WithParentID(root.ID))
	child.Messages = append(child.Messages, sidebarCostItem("worker", 2))
	grand := session.New(session.WithID("grand"), session.WithParentID(child.ID))
	grand.Messages = append(grand.Messages, sidebarCostItem("worker", 3))
	child.AddSubSession(grand)
	root.AddSubSession(child)
	tree := costTree()
	root.SetSubagentTree(&tree)
	m := newTestSidebar(t)
	for range 2 {
		m.LoadFromSession(root)
		assert.InDelta(t, 10, m.SubtreeCost(), 1e-9, "tree and embedded history are a union, not a sum")
	}
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: root.ID, AgentContext: runtime.AgentContext{AgentName: "root"}, Usage: &runtime.Usage{Cost: 7}})
	m.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: child.ID, AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{Cost: 5}})
	assert.InDelta(t, 11, m.SubtreeCost(), 1e-9, "legacy root aggregate has exact restored embedded baseline")
	m.SetSubagentTree(subagent.Snapshot{})
	assert.InDelta(t, 11, m.SubtreeCost(), 1e-9, "retained descendants survive empty attachment seeds")
	// A fresh child attachment sees its subtree, even with the root's full tree.
	m.SetSubagentContext("child-node", "root", root.ID)
	child.SetSubagentTree(&tree)
	m.LoadFromSession(child)
	assert.InDelta(t, 5, m.SubtreeCost(), 1e-9)
	m.SetSubagentContext("", "", "")
	m.LoadFromSession(session.New(session.WithID("new")))
	assert.Zero(t, m.SubtreeCost(), "switching drops the previous subtree baseline")
}
