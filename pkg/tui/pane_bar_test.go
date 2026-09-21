package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestPaneBottomBarGeometryAndPointerOwnership(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {64, 24}, {24, 12}, {1, 1}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			root.splitPane("third", "second", splitBottom)
			root.handleWindowResize(size[0], size[1])
			tree := root.panes.root
			rows := strings.Split(ansi.Strip(root.composePanes()), "\n")
			for id, r := range root.paneGeometry.Panes {
				page := &splitRecordingPage{Page: root.chatPages[id]}
				root.chatPages[id] = page
				if id == root.paneFocus() {
					root.chatPage = page
				}
				bar := root.paneHeaderHeight()
				require.Equal(t, r.H-bar, lipgloss.Height(page.TranscriptView()), "same transcript height, only its origin moves")
				if bar > 0 {
					require.GreaterOrEqual(t, r.H-bar, paneMinHeight-1)
					bottom := ansi.Cut(rows[r.Y+r.H-1], r.X, r.X+r.W)
					require.Equal(t, ansi.Strip(root.paneTitle(id, r.W)), bottom)
					require.NotContains(t, bottom, "Send to")
					require.NotContains(t, bottom, "children")
				}
				page.updates = nil
				click := tea.MouseClickMsg{X: r.X, Y: r.Y, Button: tea.MouseLeft}
				root.forwardPanePointer(click, click.X, click.Y, false)
				require.Contains(t, page.updates, click, "top cell belongs to transcript even in a split")
				page.updates = nil
				click.Y = r.Y + r.H - 1
				root.forwardPanePointer(click, click.X, click.Y, false)
				if bar > 0 {
					require.Empty(t, page.updates, "bar click cannot select a transcript message or thumb")
				} else {
					require.Contains(t, page.updates, click, "compact single pane does not reserve a bar")
				}
			}
			require.Same(t, tree, root.panes.root)
		})
	}
}

func TestPaneBottomBarFocusDoubleClickWheelAndDragStayChromeOnly(t *testing.T) {
	root := splitTestRoot(t)
	first := &splitRecordingPage{Page: root.chatPage}
	root.chatPages["profile"], root.chatPage = first, first
	root.splitPane("second", "profile", splitRight)
	root.editors["profile"].SetValue("first draft")
	root.editor.SetValue("second draft")
	r := root.paneGeometry.Panes["profile"]
	x, y := r.X+2, r.Y+r.H-1
	first.updates = nil
	root.Update(messages.WheelCoalescedMsg{X: x, Y: y, Delta: -1})
	require.Equal(t, "second", root.paneFocus(), "bar wheel never steals the composer")
	require.Empty(t, first.updates)
	for range 2 {
		root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		root.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	}
	require.Equal(t, "profile", root.paneFocus())
	require.Equal(t, "first draft", root.editor.Value())
	require.Equal(t, "second draft", root.editors["second"].Value())
	require.False(t, root.dialogMgr.Open(), "bar double-click does not rename sidebar or tab titles")
	root.Update(tea.MouseMotionMsg{X: x + 4, Y: y, Button: tea.MouseLeft})
	require.Nil(t, root.paneGesture, "pane moves still originate from tabs, not the bar")
	require.False(t, root.chatPage.IsSelecting())
	for _, msg := range first.updates {
		switch msg.(type) {
		case tea.MouseClickMsg, tea.MouseReleaseMsg, tea.MouseMotionMsg, messages.WheelCoalescedMsg:
			t.Fatalf("bar pointer leaked into transcript: %#v", msg)
		}
	}
	require.Zero(t, root.ar.ActiveCount(), "idle bar interactions acquire no animation leases")
}

type paneBarTreeRuntime struct {
	stubRuntime

	tree *subagent.Tree
}

func (r *paneBarTreeRuntime) SubagentTree() *subagent.Tree { return r.tree }

func TestPaneBarCountsDirectSubagentsAcrossStatesAndRestoration(t *testing.T) {
	root := splitTestRoot(t)
	root.sessionStates["profile"].SetCurrentAgentName("director")
	runner := root.supervisor.GetRunner("profile")
	sess := runner.App.Session()
	nodeID := subagent.SessionRootID(sess.ID)
	snapshot := subagent.Snapshot{Root: nodeID, Nodes: []subagent.NodeSnapshot{{
		Node: subagent.Node{ID: nodeID},
		Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "11111", State: subagent.NodeRunning}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "aaaaa"}}}},
			{Node: subagent.Node{ID: "22222", State: subagent.NodeCompleted}},
			{Node: subagent.Node{ID: "33333", State: subagent.NodeFailed}},
		},
	}}}
	sess.SetSubagentTree(&snapshot)
	root.splitPane("second", "profile", splitRight)
	for i := range root.tabInfos {
		if root.tabInfos[i].SessionID == "profile" {
			root.tabInfos[i].Activity = messages.TabActivityDescendantRunning
		}
	}
	require.Equal(t, 3, root.paneSubagentCount("profile", nodeID), "all direct children, excluding grandchild and unrelated views")
	require.Equal(t, 1, root.paneSubagentCount("profile", "11111"), "an attached subtree counts only its own children")
	for _, width := range []int{24, 60, 120} {
		bar := root.paneTitle("profile", width)
		require.Equal(t, width, ansi.StringWidth(bar))
		require.Contains(t, ansi.Strip(bar), "director (3)")
		require.NotContains(t, ansi.Strip(bar), "children")
		require.NotContains(t, ansi.Strip(bar), "Send to")
	}
	require.NotContains(t, ansi.Strip(root.paneTitle("second", 60)), "(0)")
	for _, width := range []int{1, 2, 3, 4, 8} {
		require.Equal(t, width, ansi.StringWidth(root.paneTitle("profile", width)))
	}
	services := &paneBarTreeRuntime{tree: subagent.NewTree()}
	runner.App = app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(services))
	require.Equal(t, 3, root.paneSubagentCount("profile", nodeID), "empty live tree preserves restored metadata")
	require.NoError(t, services.tree.Add(subagent.Node{ID: nodeID, Agent: "director"}))
	require.Zero(t, root.paneSubagentCount("profile", nodeID), "present live root with no children overrides restored tree")
	require.NoError(t, services.tree.Add(subagent.Node{ID: "44444", Parent: nodeID, Agent: "worker"}))
	require.Equal(t, 1, root.paneSubagentCount("profile", nodeID))
	require.Contains(t, ansi.Strip(root.paneTitle("profile", 60)), "director (1)")
	require.Zero(t, root.ar.ActiveCount(), "passive descendant count owns no spinner")
}
