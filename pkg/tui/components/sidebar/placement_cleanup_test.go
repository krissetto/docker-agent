package sidebar

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestPlacementCleanupRebuildsIndexesExtentAndHoverCache(t *testing.T) {
	m := New(animation.NewRuntime(), t.Context(), &service.SessionState{}).(*model)
	m.rootSessionID = "cleanup"
	m.treeCollapsed = false
	m.SetMode(ModeVertical)
	m.SetSize(40, 8)
	root := subagent.NodeSnapshot{Node: subagent.Node{ID: "root:cleanup", Agent: "root", State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: "branch", Agent: "worker", State: subagent.NodeIdle},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "leaf", Agent: "worker", State: subagent.NodeIdle}}},
	}}}
	m.SetSubagentTree(subagent.Snapshot{Root: root.Node.ID, Nodes: []subagent.NodeSnapshot{root}})
	testOpenAllSubagentBranches(m)
	cmd := m.ReconcileLayout()
	settleTreePresentation(t, m, cmd)
	require.Contains(t, m.placement.targetByID, "node:leaf")
	require.Greater(t, m.placement.extent, 0)

	m.collapsedBranches = map[subagent.NodeID]bool{"branch": true}
	m.invalidateCache()
	cmd = m.ReconcileLayout()
	settleTreePresentation(t, m, cmd)
	require.NotContains(t, m.placement.targetByID, "node:leaf", "transition cleanup drops exited-row identities")
	require.Len(t, m.placement.dynamicRows, len(refreshablePlacementIndexes(m.placement.rows)))
	require.Equal(t, placementRowsExtent(m.placement.rows), m.placement.extent, "scroll extent follows remaining rows")
	for id, index := range m.placement.targetByID {
		require.Less(t, index, len(m.placement.rows))
		require.Equal(t, id, m.placement.rows[index].id)
	}

	m.collapsedBranches["branch"] = false
	m.invalidateCache()
	settleTreePresentation(t, m, m.ReconcileLayout())
	require.Contains(t, m.placement.targetByID, "node:leaf", "later expand repopulates indexes")
}
