package dialog

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSubagentsTreeNavigationTitlesAndBounds(t *testing.T) {
	nodes := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "parent", Agent: "工作 👩‍💻", SessionID: "session", State: subagent.NodeRunning, NeedsAttention: true}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "nested", Agent: "nested", State: subagent.NodeIdle}}}}}
	for i := range 40 {
		nodes = append(nodes, subagent.NodeSnapshot{Node: subagent.Node{ID: subagent.NodeID(fmt.Sprint(i)), Agent: fmt.Sprintf("agent %02d", i)}})
	}
	d := NewSubagentsDialog(nodes, map[string]string{"session": "Generated canonical title 世界"}).(*subagentsDialog)
	d.SetSize(140, 24)
	require.Contains(t, ansi.Strip(d.View()), "Generated canonical title")
	require.Contains(t, ansi.Strip(d.View()), "attention")
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.Equal(t, subagent.NodeID("nested"), d.selectedID())
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	require.Equal(t, subagent.NodeID("parent"), d.selectedID())
	d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	require.Len(t, d.rows, 41)
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.Len(t, d.rows, 42)
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	require.Positive(t, d.scrollview.ScrollOffset())
	require.Contains(t, ansi.Strip(d.View()), "agent 39")
	d.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	x, y, _, _ := d.BodyScrollBounds()
	d.Update(tea.MouseClickMsg{X: x + d.prepared[0].chevron, Y: y, Button: tea.MouseLeft})
	require.True(t, d.collapsed["parent"])
	for _, size := range [][2]int{{80, 20}, {30, 12}, {15, 8}, {6, 4}} {
		d.SetSize(size[0], size[1])
		view := d.View()
		require.LessOrEqual(t, lipgloss.Width(view), size[0])
		require.LessOrEqual(t, lipgloss.Height(view), size[1])
		require.NotContains(t, view, "\ufffd")
	}
	nodes[0].Node.Agent = "mutated"
	require.NotContains(t, strings.Join([]string{d.View()}, ""), "mutated")
}

func TestSubagentsCanonicalIdentityBranchGuidesAndAttach(t *testing.T) {
	nodes := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "aaaaa-canonical", Agent: "worker"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "bbbbb-canonical", Agent: "worker"}}, {Node: subagent.Node{ID: "ccccc-canonical", Agent: "worker"}}}}, {Node: subagent.Node{ID: "ddddd-canonical", Agent: "worker"}}}
	d := NewSubagentsDialog(nodes, nil).(*subagentsDialog)
	d.SetSize(100, 25)
	require.Equal(t, []string{"├─", "│ ├─", "│ └─", "└─"}, []string{d.rows[0].Guides, d.rows[1].Guides, d.rows[2].Guides, d.rows[3].Guides})
	for _, id := range []string{"#aaaaa", "#bbbbb", "#ccccc", "#ddddd"} {
		require.NotContains(t, ansi.Strip(d.View()), id)
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	var attached string
	for _, msg := range collectMsgs(cmd) {
		if open, ok := msg.(messages.OpenSubagentMsg); ok {
			attached = open.NodeID
		}
	}
	require.Equal(t, "bbbbb-canonical", attached)
}

func TestSubagentsRefreshPreservesIdentityAndFallsBackToAncestor(t *testing.T) {
	nodes := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "parent", Agent: "parent"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Agent: "child"}}}}, {Node: subagent.Node{ID: "other", Agent: "other"}}}
	d := NewSubagentsDialog(nodes, nil, "child").(*subagentsDialog)
	d.SetSize(100, 30)
	require.Equal(t, subagent.NodeID("child"), d.selectedID())
	nodes = append([]subagent.NodeSnapshot{{Node: subagent.Node{ID: "new", Agent: "new"}}}, nodes...)
	d.Update(SubagentsRefreshMsg{Dialog: d, Nodes: nodes})
	require.Equal(t, subagent.NodeID("child"), d.selectedID())
	d.Update(SubagentsRefreshMsg{Dialog: d, Titles: map[string]string{"title": "loaded"}})
	require.Equal(t, subagent.NodeID("child"), d.selectedID())
	nodes[1].Children = nil
	d.Update(SubagentsRefreshMsg{Dialog: d, Nodes: nodes})
	require.Equal(t, subagent.NodeID("parent"), d.selectedID())
	d.Update(SubagentsRefreshMsg{Dialog: d})
	require.Empty(t, d.rows)
	require.Empty(t, d.selectedID())
	require.Contains(t, ansi.Strip(d.View()), "No subagents")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
}

func TestSubagentsWrapAndDisclosureNeverAttach(t *testing.T) {
	d := subagentsPresentationFixture()
	d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Equal(t, subagent.NodeID("other-child"), d.selectedID())
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, subagent.NodeID("parent-full"), d.selectedID())
	for range 2 {
		x, y := d.bodyX+d.prepared[0].chevron, d.bodyY+d.prepared[0].start
		_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		for _, msg := range collectMsgs(cmd) {
			_, opens := msg.(messages.OpenSubagentMsg)
			require.False(t, opens)
		}
	}
	require.False(t, d.collapsed["parent-full"])
}

func TestSubagentsPolicyActionDoesNotMutateUntilSaved(t *testing.T) {
	d := NewSubagentsDialog([]subagent.NodeSnapshot{{Node: subagent.Node{ID: "root", Agent: "worker"}}}, nil).(*subagentsDialog)
	d.SetSize(100, 25)
	require.Contains(t, ansi.Strip(d.View()), "Use subagents (local default): ON")
	require.Contains(t, ansi.Strip(d.View()), "Attach")
	for _, hint := range []string{"esc", "↵", "u Use"} {
		require.NotContains(t, ansi.Strip(d.View()), hint)
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	// Tab starts at the default action (Attach); move to the policy action.
	if k, _ := d.SelectedActionKey(); k.Code != 'u' {
		d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	}
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	require.Contains(t, msgs, messages.SetUseSubagentsMsg{Enabled: false})
	require.Contains(t, ansi.Strip(d.View()), "Use subagents (local default): ON", "save is not optimistic")
	id := d.selectedID()
	d.Update(SubagentsPolicyMsg{Enabled: false})
	require.Equal(t, id, d.selectedID())
	require.Len(t, d.rows, 1)
	require.Contains(t, ansi.Strip(d.View()), "Use subagents (local default): OFF")
	require.Contains(t, ansi.Strip(d.View()), "worker")

	view := d.View()
	row, col := d.Position()
	dl := NewDialogLayout(view, row, col)
	found := false
	for y := row; y < row+lipgloss.Height(view); y++ {
		for x := col; x < col+lipgloss.Width(view); x++ {
			if key, hit := d.ActionKeyAt(x, y, dl); hit && key.Code == 'u' {
				// The shared dialog manager routes this measured mouse target as a key.
				_, cmd = d.Update(key)
				require.Contains(t, collectMsgs(cmd), messages.SetUseSubagentsMsg{Enabled: true})
				found = true
			}
		}
	}
	require.True(t, found)
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Contains(t, collectMsgs(cmd), messages.OpenSubagentMsg{NodeID: "root"}, "Attach survives policy changes")
}

func TestSubagentsPolicyMouseUsesSharedManagerAndRefreshesCache(t *testing.T) {
	mgr := New(newDialogRuntime()).(*manager)
	mgr.SetSize(100, 25)
	d := NewSubagentsDialog(nil, nil)
	mgr.Update(OpenDialogMsg{Model: d})
	settleTestDialog(mgr)
	require.Contains(t, ansi.Strip(mgr.View()), "Use subagents (local default): ON")
	x, y := familyActionCell(t, d, "Use subagents (local default): ON")
	_, cmd := mgr.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Contains(t, collectMsgs(cmd), messages.SetUseSubagentsMsg{Enabled: false})
	require.Contains(t, ansi.Strip(mgr.View()), "Use subagents (local default): ON")
	mgr.Update(SubagentsPolicyMsg{Enabled: false})
	require.Contains(t, ansi.Strip(mgr.View()), "Use subagents (local default): OFF")
	require.Contains(t, ansi.Strip(mgr.View()), "No subagents")
}
