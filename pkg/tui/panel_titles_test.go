package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/dialog"
)

func TestPanelTitlesMetadataOnlyCapturedIDsAndResetFence(t *testing.T) {
	root := panelFixture(t)
	sess := root.application.Session()
	rt := &sourceRuntimeFixture{lifecycleSessions: newLifecycleSessions(), metadata: []runtime.SessionSummaryEntry{{SessionID: "child-session", Title: "Cached generated title 世界"}, {SessionID: "unrelated", Title: "Must not leak"}}}
	application := app.New(t.Context(), rt, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(stubRuntime{}))
	root.application = application
	root.supervisor.GetRunner("profile").App = application
	data := root.panelOwnerData("profile")
	data.treeNodes = []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", Agent: "worker", SessionID: "child-session"}}}
	d := dialog.NewSubagentsDialog(data.treeNodes, nil)
	root.updateDialogCmd(dialog.OpenDialogMsg{Model: d})
	cmd := root.loadPanelTitles(d, data)
	require.Zero(t, rt.listings.Load(), "metadata reads stay off owner loop")
	result := cmd().(panelTitlesMsg)
	require.EqualValues(t, 1, rt.listings.Load())
	require.Zero(t, rt.prepares.Load(), "title lookup never prepares or attaches sessions")
	require.NotContains(t, result.titles, "unrelated")
	root.acceptPanelTitles(result)
	require.Contains(t, ansi.Strip(d.View()), "Cached generated title")
	root.ingestPanelEvent("profile", &app.SessionResetEvent{})
	result.titles["child-session"] = "stale title"
	root.acceptPanelTitles(result)
	require.NotContains(t, ansi.Strip(d.View()), "stale title")
}

func TestPanelTitlesDismissCancellationAndOwnerFence(t *testing.T) {
	root := panelFixture(t)
	data := root.panelData["profile"]
	nodes := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Agent: "worker", SessionID: "child-session"}}}
	d := dialog.NewSubagentsDialog(nodes, nil)
	cancelled := false
	d.(interface{ SetCancel(func()) }).SetCancel(func() { cancelled = true })
	root.updateDialogCmd(dialog.OpenDialogMsg{Model: d})
	result := panelTitlesMsg{data: data, application: root.application, owner: "another-owner", sessionID: data.sessionID, generation: data.generation, dialog: d, titles: map[string]string{"child-session": "Wrong owner title"}}
	root.acceptPanelTitles(result)
	require.NotContains(t, ansi.Strip(d.View()), "Wrong owner title")
	dialog.CleanupDialog(d)
	require.True(t, cancelled, "dismissal cleanup cancels metadata context")
	replacement := dialog.NewSubagentsDialog(nodes, nil)
	root.updateDialogCmd(dialog.OpenDialogMsg{Model: replacement})
	result.owner = "profile"
	root.acceptPanelTitles(result)
	require.NotContains(t, ansi.Strip(replacement.View()), "Wrong owner title", "result must match exact dialog instance")
}

func TestPanelTitlesIncludesAncestorAndSiblingOfAttachedView(t *testing.T) {
	root := panelFixture(t)
	sess := root.application.Session()
	rt := &sourceRuntimeFixture{lifecycleSessions: newLifecycleSessions(), metadata: []runtime.SessionSummaryEntry{{SessionID: "ancestor-session", Title: "Ancestor generated title"}, {SessionID: "sibling-session", Title: "Sibling generated title"}}}
	application := app.New(t.Context(), rt, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}), app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "child"}))
	root.application = application
	root.supervisor.GetRunner("profile").App = application
	snapshot := panelTree("ancestor-session", subagent.NodeIdle).Snapshot
	snapshot.Nodes[0].Children[0].Node.SessionID = sess.ID
	snapshot.Nodes[0].Children = append(snapshot.Nodes[0].Children, subagent.NodeSnapshot{Node: subagent.Node{ID: "sibling", Agent: "sibling", SessionID: "sibling-session"}})
	application.Session().SetSubagentTree(&snapshot)
	data := root.panelOwnerData("profile")
	d := dialog.NewSubagentsDialog(data.treeNodes, nil, "child")
	root.updateDialogCmd(dialog.OpenDialogMsg{Model: d})
	cmd := root.loadPanelTitles(d, data)
	result := cmd().(panelTitlesMsg)
	require.Contains(t, result.titles, "ancestor-session")
	require.Contains(t, result.titles, "sibling-session")
	root.acceptPanelTitles(result)
	require.Contains(t, ansi.Strip(d.View()), "Ancestor generated title")
	require.Contains(t, ansi.Strip(d.View()), "Sibling generated title")
	require.Zero(t, rt.prepares.Load(), "inspection never prepares or submits to a session")
}
