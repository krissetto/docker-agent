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

func TestSubagentsDistinctIdentityBranchGuidesAndAttach(t *testing.T) {
	nodes := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "aaaaa-canonical", Agent: "worker"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "bbbbb-canonical", Agent: "worker"}}, {Node: subagent.Node{ID: "ccccc-canonical", Agent: "worker"}}}}, {Node: subagent.Node{ID: "ddddd-canonical", Agent: "worker"}}}
	d := NewSubagentsDialog(nodes, nil).(*subagentsDialog)
	d.SetSize(100, 25)
	require.Equal(t, []string{"├─", "│ ├─", "│ └─", "└─"}, []string{d.rows[0].Guides, d.rows[1].Guides, d.rows[2].Guides, d.rows[3].Guides})
	for _, id := range []string{"#aaaaa", "#bbbbb", "#ccccc", "#ddddd"} {
		require.Contains(t, ansi.Strip(d.View()), id)
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
