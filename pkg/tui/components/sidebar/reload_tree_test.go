package sidebar

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestReloadAuthoritativeRootTopologyAndNestedActions(t *testing.T) {
	t.Parallel()
	for _, state := range []subagent.NodeState{subagent.NodeRunning, subagent.NodeIdle, subagent.NodeStopped} {
		t.Run(string(state), func(t *testing.T) {
			m := newSubagentTestModel(t)
			m.SetSize(80, 40)
			sess := session.New(session.WithID("reload-session"))
			rootID := subagent.SessionRootID(sess.ID)
			grandchild := subagent.NodeSnapshot{Node: subagent.Node{ID: "full-grandchild-node-id", Agent: "reviewer", SessionID: "grandchild-session", Parent: "full-child-node-id", State: subagent.NodeFailed, NeedsAttention: true}}
			child := subagent.NodeSnapshot{Node: subagent.Node{ID: "full-child-node-id", Agent: "worker", SessionID: "child-session", Parent: rootID, State: state}, Children: []subagent.NodeSnapshot{grandchild}}
			topology := subagent.Snapshot{Root: rootID, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: rootID, Agent: "root", SessionID: sess.ID, State: subagent.NodeIdle}, Children: []subagent.NodeSnapshot{child}}}}
			sess.SetSubagentTree(&topology)
			// A main reload explicitly leaves any previous attached perspective.
			m.SetSubagentContext("previous-child", "previous-parent", "previous-session")
			m.SetSubagentContext("", "", "")
			m.LoadFromSession(sess)
			assert.Equal(t, rootID, m.treeRootID())
			assert.Equal(t, state == subagent.NodeRunning, m.subagentSpinnerOn)
			assertReloadTreeActions(t, m)
			// Initial empty/foreign live publications cannot erase this restored owner.
			m.SetSubagentTree(subagent.Snapshot{})
			m.SetSubagentTree(subagent.Snapshot{Root: "root:foreign", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root:foreign", Agent: "root"}}}})
			assertReloadTreeActions(t, m)
			// A same-session authoritative update preserves IDs while replacing state.
			topology.Nodes[0].Children[0].Node.State = subagent.NodeIdle
			m.SetSubagentTree(topology)
			assert.False(t, m.subagentSpinnerOn)
			assertReloadTreeActions(t, m)
		})
	}
}

func assertReloadTreeActions(t *testing.T, m *model) {
	t.Helper()
	view := ansi.Strip(m.View())
	assert.Contains(t, view, "worker")
	assert.Contains(t, view, "reviewer")
	assert.Contains(t, view, "failed")
	for _, want := range []subagent.NodeID{"full-child-node-id", "full-grandchild-node-id"} {
		found := false
		for row, id := range m.subagentHoverZone {
			if id != want {
				continue
			}
			result, payload := m.HandleClickType(m.layoutCfg.PaddingLeft+4, row)
			assert.Equal(t, ClickSubagent, result)
			assert.Equal(t, string(want), payload)
			found = true
		}
		require.True(t, found, "restored full node ID has an attach action")
	}
}

func TestReloadAttachedLeafUsesCanonicalNodeNotMainRoot(t *testing.T) {
	t.Parallel()
	m := newSubagentTestModel(t)
	m.SetSize(80, 40)
	sess := session.New(session.WithID("leaf-session"))
	root := subagent.SessionRootID("main-session")
	topology := subagent.Snapshot{Root: root, Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: root, Agent: "root", SessionID: "main-session"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "leaf-full-id", Agent: "leaf", SessionID: sess.ID, Parent: root, State: subagent.NodeIdle}}}}}}
	sess.SetSubagentTree(&topology)
	m.SetSubagentContext("leaf-full-id", "root", "main-session")
	m.LoadFromSession(sess)
	m.View()
	assert.Equal(t, subagent.NodeID("leaf-full-id"), m.treeRootID())
	assert.Empty(t, m.subagentNodes)
	assert.Empty(t, m.treeControls, "restored leaf has no child-collapse control")
	assert.Empty(t, m.agentClickZones)
	assert.Empty(t, m.subagentHoverZone)
	assert.NotContains(t, ansi.Strip(m.View()), "0 total")
	for line := range strings.SplitSeq(ansi.Strip(m.View()), "\n") {
		assert.NotContains(t, line, "▶ root", "main root is not fabricated under the parent link")
	}
}
