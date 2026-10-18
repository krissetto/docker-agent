package tui

import (
	"math"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func workspaceIDs(root *appModel) []string {
	var ids []string
	for _, owner := range root.paneWorkspaces.order {
		ids = append(ids, owner.anchor)
	}
	return ids
}

func paneTitlePoint(t *testing.T, root *appModel, id string) (int, int) {
	t.Helper()
	r := root.paneAreas(root.paneGeometry.Panes[id]).title
	require.Positive(t, r.H)
	return r.X + min(3, r.W-1), r.Y
}

func dragWorkspacePointer(root *appModel, x, y, dx, dy int) tea.Cmd {
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: dx, Y: dy, Button: tea.MouseLeft})
	_, cmd := root.Update(tea.MouseReleaseMsg{X: dx, Y: dy, Button: tea.MouseLeft})
	return cmd
}

func TestWorkspaceTabsOnePerGroupRestoreFocusAndLayout(t *testing.T) {
	root := splitTestRoot(t)
	root.editor.SetValue("first draft")
	root.splitPane("second", "profile", splitRight)
	root.editor.SetValue("second draft")
	root.handleSwitchTab("profile")
	d := root.paneGeometry.Dividers[0]
	root.handlePaneAction(messages.PaneActionMsg{Action: "resize", DividerID: d.ID})
	root.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	before := root.panes
	require.Equal(t, 2, root.tabBar.Count())
	require.Equal(t, []string{"profile", "third"}, workspaceIDs(root))
	plain := ansi.Strip(root.tabBar.View())
	require.Contains(t, plain, "[2]")
	require.Equal(t, 2, strings.Count(plain, "×"), "every workspace has a close button")
	root.Update(messages.SwitchTabMsg{SessionID: "third", Workspace: true})
	require.Equal(t, []string{"third"}, root.panes.Sessions())
	root.Update(messages.SwitchTabMsg{SessionID: "profile", Workspace: true})
	require.Equal(t, "profile", root.paneFocus())
	require.Equal(t, "first draft", root.editor.Value())
	require.Equal(t, before, root.panes)
	root.handleSwitchTab("second")
	root.Update(messages.SwitchTabMsg{SessionID: "third", Workspace: true})
	root.Update(messages.SwitchTabMsg{SessionID: "profile", Workspace: true})
	require.Equal(t, "second", root.paneFocus())
	require.Equal(t, "second draft", root.editor.Value())
}

func TestWorkspacePaneTitleDetachAtStripInsertionKeepsLiveOwners(t *testing.T) {
	for _, source := range []string{"profile", "second"} {
		for _, position := range []int{0, 1, 2} {
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			root.handleSwitchTab(source)
			root.editor.SetValue("live exact draft")
			runner, page, ed := root.supervisor.GetRunner(source), root.chatPages[source], root.editors[source]
			generation, _ := root.supervisor.RouteGeneration(source)
			x, y := paneTitlePoint(t, root, source)
			destX := tabFrameOrigin()
			if position == 1 {
				destX, _ = paneTabPoint(t, root, "third")
			}
			if position == 2 {
				destX = root.width - 10
			}
			destY := root.composerLayout().tabsTop
			root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.True(t, root.paneGesture.fromPane)
			before := root.panes.root
			root.Update(tea.MouseMotionMsg{X: destX, Y: destY, Button: tea.MouseLeft})
			require.Same(t, before, root.panes.root, "preview does not edit layout")
			require.NotNil(t, root.paneGestureLayer(), "insertion marker is a valid target")
			root.Update(tea.MouseReleaseMsg{X: destX, Y: destY, Button: tea.MouseLeft})
			require.Equal(t, source, root.paneFocus())
			require.Equal(t, []string{source}, root.panes.Sessions())
			require.Equal(t, "live exact draft", root.editor.Value())
			require.Same(t, runner, root.supervisor.GetRunner(source))
			require.Same(t, page, root.chatPages[source])
			require.Same(t, ed, root.editors[source])
			now, _ := root.supervisor.RouteGeneration(source)
			require.Equal(t, generation, now)
			require.Equal(t, 3, root.supervisor.Count())
			require.Equal(t, 3, root.tabBar.Count())
			require.Equal(t, source, workspaceIDs(root)[position])
			remaining := "profile"
			if source == remaining {
				remaining = "second"
			}
			root.handleSwitchTab(remaining)
			require.Equal(t, []string{remaining}, root.panes.Sessions())
			require.Len(t, root.paneGeometry.Panes, 1)
			require.Zero(t, root.paneHeaderHeight(), "lone remaining pane fills without split chrome")
			requireWorkspaceMembership(t, root)
		}
	}
}

func TestWorkspacePaneDetachAndRearrangeInvalidDropsAreNoOps(t *testing.T) {
	for _, action := range []string{"center", "composer", "plus", "margin", "escape", "blur", "resize", "self"} {
		t.Run(action, func(t *testing.T) {
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			root.editor.SetValue("keep draft")
			x, y := paneTitlePoint(t, root, "profile")
			before, order := root.panes.root, workspaceIDs(root)
			root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			r := root.paneGeometry.Panes["second"]
			dx, dy := r.X+r.W/2, r.Y+r.H/2
			switch action {
			case "composer":
				dx, dy = 4, root.height-2
			case "plus":
				plain := ansi.Strip(root.tabBar.View())
				plusIndex := strings.Index(plain, "+")
				require.NotEqual(t, -1, plusIndex)
				dx, dy = tabFrameOrigin()+ansi.StringWidth(plain[:plusIndex]), root.composerLayout().tabsTop
			case "margin":
				dx, dy = tabFrameOrigin()-1, root.composerLayout().tabsTop
			case "self":
				dx, dy = edgePoint(root.paneGeometry.Panes["profile"], splitLeft)
			}
			root.Update(tea.MouseMotionMsg{X: dx, Y: dy, Button: tea.MouseLeft})
			switch action {
			case "escape":
				root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			case "blur":
				root.Update(tea.BlurMsg{})
			case "resize":
				root.Update(tea.WindowSizeMsg{Width: 121, Height: 40})
			}
			root.Update(tea.MouseReleaseMsg{X: dx, Y: dy, Button: tea.MouseLeft})
			require.Same(t, before, root.panes.root)
			require.Equal(t, order, workspaceIDs(root))
			require.Equal(t, "second", root.paneFocus())
			require.Equal(t, "keep draft", root.editor.Value())
		})
	}
}

func TestWorkspacePaneTitleEdgeRearrangesWithoutNewWorkspace(t *testing.T) {
	for _, edge := range []splitEdge{splitLeft, splitRight, splitTop, splitBottom} {
		root := splitTestRoot(t)
		root.splitPane("second", "profile", splitRight)
		root.editors["profile"].SetValue("profile draft")
		x, y := paneTitlePoint(t, root, "profile")
		dx, dy := edgePoint(root.paneGeometry.Panes["second"], edge)
		dragWorkspacePointer(root, x, y, dx, dy)
		require.Equal(t, 2, root.tabBar.Count())
		require.Len(t, root.panes.Sessions(), 2)
		require.Equal(t, "profile", root.paneFocus())
		require.Equal(t, "profile draft", root.editor.Value())
		requireWorkspaceMembership(t, root)
	}
}

func TestWorkspaceTabDragMovesWholeGroupAndPreservesInternalRatio(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.handleWindowResize(220, 70)
	d := root.paneGeometry.Dividers[0]
	root.handlePaneAction(messages.PaneActionMsg{Action: "resize", DividerID: d.ID})
	root.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	ratio := math.Float64bits(root.panes.root.ratio)
	root.handleSwitchTab("third")
	x, y := paneTabPoint(t, root, "profile")
	dx, dy := edgePoint(root.paneGeometry.Panes["third"], splitRight)
	dragWorkspacePointer(root, x, y, dx, dy)
	require.Equal(t, []string{"third", "profile", "second"}, root.panes.Sessions())
	require.Equal(t, "second", root.paneFocus(), "workspace drag preserves remembered pane focus")
	require.Equal(t, ratio, math.Float64bits(root.panes.root.second.ratio))
	require.Equal(t, 1, root.tabBar.Count())
	require.Equal(t, 3, root.supervisor.Count())
	requireWorkspaceMembership(t, root)
}

func TestWorkspaceGroupedCloseOnlyFocusedSessionAndHideIsRecoverable(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.splitPane("third", "profile", splitBottom)
	require.Equal(t, 1, root.tabBar.Count())
	x, y := paneTabPoint(t, root, "profile")
	_, cmd := root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseMiddle})
	_, closed := firstOfType[messages.CloseTabMsg](collectMsgs(cmd))
	require.False(t, closed, "middle click cannot close a group")
	root.singlePane()
	require.Equal(t, 1, root.tabBar.Count())
	require.Len(t, root.panes.Sessions(), 1)
	require.Len(t, root.paneWorkspaces.members(root.paneWorkspaces.active), 3)
	root.handlePaneAction(messages.PaneActionMsg{Action: "focus", Source: "second"})
	require.Len(t, root.panes.Sessions(), 3, "Focus restores hidden group without detaching")
	root.focusedPanel = PanelContent
	_, cmd = root.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	closeMsg, ok := firstOfType[messages.CloseTabMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.Equal(t, "second", closeMsg.SessionID)
	root.Update(closeMsg)
	require.Nil(t, root.supervisor.GetRunner("second"))
	require.NotNil(t, root.supervisor.GetRunner("profile"))
	require.NotNil(t, root.supervisor.GetRunner("third"))
	require.Len(t, root.panes.Sessions(), 2)
	require.Equal(t, 1, root.tabBar.Count())
	requireWorkspaceMembership(t, root)
}

func TestWorkspaceGroupStripReorderKeepsMembershipAndFocus(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	x, y := paneTabPoint(t, root, "profile")
	dx, _ := paneTabPoint(t, root, "third")
	before := root.panes
	cmd := dragWorkspacePointer(root, x, y, dx+12, y)
	reorder, ok := firstOfType[messages.ReorderTabMsg](collectMsgs(cmd))
	require.True(t, ok)
	root.Update(reorder)
	require.Equal(t, []string{"third", "profile"}, workspaceIDs(root))
	require.Equal(t, before, root.panes)
	require.Equal(t, "second", root.paneFocus())
	require.Equal(t, 2, root.tabBar.Count())
	tabs, active := root.supervisor.GetTabs()
	root.Update(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
	require.Equal(t, []string{"third", "profile"}, workspaceIDs(root), "supervisor refresh cannot undo workspace reorder")
	_, cmd = root.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	next, ok := firstOfType[messages.SwitchTabMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.True(t, next.Workspace)
	root.Update(next)
	require.Equal(t, "third", root.paneFocus())
	_, cmd = root.Update(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl})
	next, ok = firstOfType[messages.SwitchTabMsg](collectMsgs(cmd))
	require.True(t, ok)
	root.Update(next)
	require.Equal(t, "second", root.paneFocus())
	require.Equal(t, before, root.panes)
}

func TestActualProgramWorkspacePaneDetachPreservesDraftAndGroup(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.editors["profile"].SetValue("detached running draft")
	root.editor.SetValue("remaining group draft")
	x, y := paneTitlePoint(t, root, "profile")
	dx, dy := root.width-10, root.composerLayout().tabsTop
	program := startPaneProgram(t, root)
	program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	motion := tea.MouseMotionMsg{X: dx, Y: dy, Button: tea.MouseLeft}
	program.Send(messages.PointerBoundaryMsg{Pending: messages.PointerUpdateMsg{X: dx, Y: dy, Motion: &motion}, Event: tea.MouseReleaseMsg{X: dx, Y: dy, Button: tea.MouseLeft}})
	snapshot := queryPaneProgram(t, program)
	require.Equal(t, "profile", snapshot.focus)
	require.Equal(t, "detached running draft", snapshot.draft)
	require.Equal(t, []string{"profile"}, snapshot.panes)
	program.Send(messages.SwitchTabMsg{SessionID: "second", Workspace: true})
	snapshot = queryPaneProgram(t, program)
	require.Equal(t, "second", snapshot.focus)
	require.Equal(t, "remaining group draft", snapshot.draft)
	require.Equal(t, []string{"second"}, snapshot.panes)
	require.False(t, snapshot.gesture)
}

func TestWorkspaceDetachFromThreePaneGroupKeepsRemainingRatiosAndDrafts(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.splitPane("third", "profile", splitBottom)
	root.editors["profile"].SetValue("first still running")
	root.editors["second"].SetValue("second still running")
	root.editor.SetValue("third live draft")
	remaining, ok := root.panes.Remove("third")
	require.True(t, ok)
	x, y := paneTitlePoint(t, root, "third")
	dragWorkspacePointer(root, x, y, root.width-10, root.composerLayout().tabsTop)
	require.Equal(t, "third", root.paneFocus())
	require.Equal(t, "third live draft", root.editor.Value())
	require.Equal(t, 2, root.tabBar.Count())
	root.Update(messages.SwitchTabMsg{SessionID: "profile", Workspace: true})
	require.Equal(t, remaining, root.panes)
	require.Equal(t, "profile", root.paneFocus())
	require.Equal(t, "first still running", root.editor.Value())
	root.handleSwitchTab("second")
	require.Equal(t, "second still running", root.editor.Value())
	requireWorkspaceMembership(t, root)
}
