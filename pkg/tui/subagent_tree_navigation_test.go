package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestShowSubagentsOpensRootedTreeWithoutVisiblePanel(t *testing.T) {
	root := panelFixture(t)
	root.applyPanelSettings(messages.PanelSettings{Elements: []messages.PanelElement{}})
	event := panelTree("profile", subagent.NodeIdle)
	event.Snapshot.Nodes[0].Node.Agent = "director"
	event.Snapshot.Nodes[0].Children[0].Node.SessionID = "child-session"
	root.ingestPanelEvent("profile", event)
	_, cmd := root.Update(messages.ShowSubagentSessionsMsg{})
	msgs := collectMsgs(cmd)
	opened, ok := firstOfType[dialog.OpenDialogMsg](msgs)
	require.True(t, ok)
	require.Same(t, root.panelData["profile"].treeDialog, opened.Model)
	opened.Model.SetSize(100, 30)
	view := ansi.Strip(opened.Model.View())
	require.Contains(t, view, "Subagents")
	require.Contains(t, view, "director")
	require.Contains(t, view, "worker")
	require.NotContains(t, view, "Commands")
	require.NotContains(t, view, "choose")
	require.NotContains(t, view, "↵")
	_, attached := firstOfType[messages.OpenSubagentMsg](msgs)
	require.False(t, attached, "opening the inspector never attaches or sends")
	root.updateDialogCmd(opened)
	event.Snapshot.Nodes[0].Children[0].Node.Agent = "refreshed worker"
	root.ingestPanelEvent("profile", event)
	require.Contains(t, ansi.Strip(opened.Model.View()), "refreshed worker")
	_, cmd = opened.Model.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	_, attached = firstOfType[messages.OpenSubagentMsg](collectMsgs(cmd))
	require.False(t, attached)
	_, cmd = opened.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	attach, ok := firstOfType[messages.OpenSubagentMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.Equal(t, "child", attach.NodeID)
}

func TestShowSubagentsAttachedContextKeepsAncestorsAndSelectsCanonicalNode(t *testing.T) {
	root := panelFixture(t)
	sess := root.application.Session()
	tree := panelTree("ancestor-session", subagent.NodeIdle).Snapshot
	tree.Nodes[0].Node.Agent = "ancestor"
	tree.Nodes[0].Children[0].Node.SessionID = sess.ID
	tree.Nodes[0].Children = append(tree.Nodes[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: "sibling", Agent: "sibling"}})
	tree.Nodes[0].Children[0].Children = []subagent.NodeSnapshot{{Node: subagent.Node{ID: "grandchild", Agent: "grandchild"}}}
	sess.SetSubagentTree(&tree)
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}), app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "child"}))
	root.application = application
	root.supervisor.GetRunner("profile").App = application
	opened, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(root.showSubagentSessions()))
	require.True(t, ok)
	opened.Model.SetSize(100, 30)
	view := ansi.Strip(opened.Model.View())
	for _, name := range []string{"ancestor", "sibling", "grandchild"} {
		require.Contains(t, view, name)
	}
	require.Len(t, root.panelData["profile"].nodes, 1, "panel counts remain descendant scoped")
	_, cmd := opened.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	attach, ok := firstOfType[messages.OpenSubagentMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.Equal(t, "child", attach.NodeID)
}

func TestShowSubagentsEmptySessionStillOpensTree(t *testing.T) {
	root := panelFixture(t)
	opened, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(root.showSubagentSessions()))
	require.True(t, ok)
	opened.Model.SetSize(80, 24)
	require.Contains(t, ansi.Strip(opened.Model.View()), "No subagents")
	_, cmd := opened.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
}

func TestSubagentsRestoredTreeSurvivesUntilLiveThenClears(t *testing.T) {
	root := panelFixture(t)
	sess := root.application.Session()
	restored := panelTree(sess.ID, subagent.NodeIdle).Snapshot
	sess.SetSubagentTree(&restored)
	services := &paneBarTreeRuntime{tree: subagent.NewTree()}
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(services))
	root.application = application
	root.supervisor.GetRunner("profile").App = application
	opened, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(root.showSubagentSessions()))
	require.True(t, ok)
	opened.Model.SetSize(100, 30)
	require.Contains(t, ansi.Strip(opened.Model.View()), "worker", "empty registry before restoration must preserve persisted tree")
	data := root.panelData["profile"]
	require.False(t, data.treeLiveObserved)
	root.ingestPanelEvent("profile", &runtime.SubagentTreeEvent{})
	require.NotEmpty(t, data.treeNodes, "empty seed before first live match preserves restored tree")
	root.updateDialogCmd(opened)
	root.ingestPanelEvent("profile", &runtime.SubagentTreeEvent{Snapshot: restored})
	require.True(t, data.treeLiveObserved)
	foreign := panelTree("unrelated", subagent.NodeIdle).Snapshot
	root.ingestPanelEvent("profile", &runtime.SubagentTreeEvent{Snapshot: foreign})
	require.Empty(t, data.treeNodes, "known-live omission clears even when other roots remain")
	require.Contains(t, ansi.Strip(opened.Model.View()), "No subagents")
	root.ingestPanelEvent("profile", &runtime.SubagentTreeEvent{Snapshot: restored})
	root.ingestPanelEvent("profile", &runtime.SubagentTreeEvent{})
	require.Empty(t, data.treeNodes)
	require.Contains(t, ansi.Strip(opened.Model.View()), "No subagents")
}

func TestSubagentsRemovedAttachedChildRetainsEnclosingRoot(t *testing.T) {
	root := panelFixture(t)
	sess := root.application.Session()
	tree := panelTree("ancestor-session", subagent.NodeIdle).Snapshot
	tree.Nodes[0].Node.Agent = "ancestor"
	tree.Nodes[0].Children[0].Node.SessionID = sess.ID
	sess.SetSubagentTree(&tree)
	application := app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}), app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "child"}))
	root.application = application
	root.supervisor.GetRunner("profile").App = application
	opened, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(root.showSubagentSessions()))
	require.True(t, ok)
	root.updateDialogCmd(opened)
	tree.Nodes[0].Children = nil
	root.ingestPanelEvent("profile", &runtime.SubagentTreeEvent{Snapshot: tree})
	require.Contains(t, ansi.Strip(opened.Model.View()), "ancestor")
	require.NotContains(t, ansi.Strip(opened.Model.View()), "worker")
	_, cmd := opened.Model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	attached, ok := firstOfType[messages.OpenSubagentMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.Equal(t, string(tree.Nodes[0].Node.ID), attached.NodeID)
}
