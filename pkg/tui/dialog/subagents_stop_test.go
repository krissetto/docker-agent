package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestSubagentsStopRequiresConfirmation(t *testing.T) {
	nodes := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Parent: "root"}}}}}
	d := NewSubagentsDialog(nodes, nil).(*subagentsDialog)
	d.SetSize(120, 25)
	_, cmd := d.Update(tea.KeyPressMsg{Code: 's'})
	require.Empty(t, collectMsgs(cmd))
	require.Empty(t, d.stopTarget)
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd = d.Update(tea.KeyPressMsg{Code: 's'})
	require.Empty(t, collectMsgs(cmd))
	require.Equal(t, subagent.NodeID("child"), d.stopTarget)
	require.Contains(t, d.View(), "Permanently stop")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Empty(t, collectMsgs(cmd))
	require.Empty(t, d.stopTarget)
	d.Update(tea.KeyPressMsg{Code: 's'})
	_, cmd = d.Update(tea.KeyPressMsg{Code: 'y'})
	require.Contains(t, collectMsgs(cmd), messages.StopSubagentSubtreeMsg{NodeID: "child"})
	d.Update(tea.KeyPressMsg{Code: 's'})
	d.Update(SubagentsRefreshMsg{Dialog: d, Nodes: nil})
	_, cmd = d.Update(tea.KeyPressMsg{Code: 'y'})
	require.Empty(t, collectMsgs(cmd))
}

func TestSubagentsUnavailableControlsAndPinnedIdentity(t *testing.T) {
	nodes := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-unique", Name: "\x1b[31mWorker\nname", Parent: "root"}}}}}
	d := NewSubagentsDialog(nodes, nil, "child-unique").(*subagentsDialog)
	d.SetSize(140, 30)
	d.Update(tea.KeyPressMsg{Code: 's'})
	nodes[0].Children[0].Node.Name = "renamed"
	d.Update(SubagentsRefreshMsg{Dialog: d, Nodes: nodes})
	require.Contains(t, d.View(), "Worker name (child-unique)")
	require.NotContains(t, d.stopIdentity, "\x1b")
	d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	d.Update(SubagentsCapabilitiesMsg{})
	for _, key := range []rune{'u', 's'} {
		_, cmd := d.Update(tea.KeyPressMsg{Code: key})
		require.Empty(t, collectMsgs(cmd))
	}
	require.Contains(t, d.View(), "Use subagents: unavailable")
	require.Contains(t, d.View(), "Stop subtree: unavailable")
	require.Empty(t, d.stopTarget)
	require.Equal(t, subagent.NodeID("child-unique"), d.selectedID())
}
