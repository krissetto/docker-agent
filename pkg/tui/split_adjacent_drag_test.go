package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func edgePoint(r splitRect, edge splitEdge) (int, int) {
	switch edge {
	case splitLeft:
		return r.X, r.Y + r.H/2
	case splitRight:
		return r.X + r.W - 1, r.Y + r.H/2
	case splitTop:
		return r.X + r.W/2, r.Y
	default:
		return r.X + r.W/2, r.Y + r.H - 1
	}
}
func TestFocusedTabDragChoosesAdjacentSourceTab(t *testing.T) {
	for _, reordered := range []bool{false, true} {
		for _, source := range []string{"profile", "second", "third"} {
			for _, edge := range []splitEdge{splitLeft, splitRight, splitTop, splitBottom} {
				root := splitTestRoot(t)
				if reordered {
					root.supervisor.ReorderTab(0, 2)
				}
				tabs, active := root.supervisor.GetTabs()
				root.setTabs(tabs, active)
				root.handleSwitchTab(source)
				root.editor.SetValue("dragged exact draft")
				order := root.paneOrder()
				index := 0
				for i, id := range order {
					if id == source {
						index = i
					}
				}
				neighbor := index + 1
				if neighbor == len(order) {
					neighbor = index - 1
				}
				x, y := paneTabPoint(t, root, source)
				root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				require.Equal(t, order, root.paneGesture.order)
				r := root.paneGesture.geometry.Panes[source]
				dx, dy := edgePoint(r, edge)
				root.Update(tea.MouseMotionMsg{X: dx, Y: dy, Button: tea.MouseLeft})
				require.NotNil(t, root.paneGestureLayer())
				root.Update(tea.MouseReleaseMsg{X: dx, Y: dy, Button: tea.MouseLeft})
				want := []string{order[neighbor], source}
				if edge == splitLeft || edge == splitTop {
					want = []string{source, order[neighbor]}
				}
				require.Equal(t, want, root.panes.Sessions(), "order=%v source=%s edge=%v", order, source, edge)
				require.Equal(t, source, root.paneFocus())
				require.Equal(t, "dragged exact draft", root.editor.Value())
				require.Equal(t, order, root.paneOrder())
			}
		}
	}
}
func TestFocusedTabDragSkipsClosedNeighborUsingCapturedOrder(t *testing.T) {
	for _, source := range []string{"profile", "second", "third"} {
		root := splitTestRoot(t)
		root.handleSwitchTab(source)
		x, y := paneTabPoint(t, root, source)
		root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		captured := append([]string(nil), root.paneGesture.order...)
		closed, want := "second", "third"
		if source == "second" {
			closed, want = "third", "profile"
		}
		if source == "third" {
			closed, want = "second", "profile"
		}
		root.closeTab(closed)
		tabs, active := root.supervisor.GetTabs()
		root.Update(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: active})
		require.NotNil(t, root.paneGesture, "hidden neighbor removal need not abort a focused drag")
		require.Equal(t, captured, root.paneGesture.order, "press-time identities remain authoritative")
		r := root.paneGesture.geometry.Panes[source]
		dx, dy := edgePoint(r, splitRight)
		root.Update(tea.MouseMotionMsg{X: dx, Y: dy, Button: tea.MouseLeft})
		root.Update(tea.MouseReleaseMsg{X: dx, Y: dy, Button: tea.MouseLeft})
		require.Equal(t, []string{want, source}, root.panes.Sessions())
		require.Equal(t, source, root.paneFocus())
	}
}
func TestFocusedTabDragRejectsChangedOrderAndMissingAlternatives(t *testing.T) {
	for _, change := range []string{"reorder", "source-close", "only-tab"} {
		root := splitTestRoot(t)
		root.handleSwitchTab("third")
		if change == "only-tab" {
			root.closeTab("profile")
			root.closeTab("second")
		}
		x, y := paneTabPoint(t, root, "third")
		root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		original := root.panes.root
		r := root.paneGesture.geometry.Panes["third"]
		dx, dy := edgePoint(r, splitLeft)
		switch change {
		case "reorder":
			root.supervisor.ReorderTab(0, 1)
		case "source-close":
			root.supervisor.CloseSession("third")
		}
		root.Update(tea.MouseMotionMsg{X: dx, Y: dy, Button: tea.MouseLeft})
		root.Update(tea.MouseReleaseMsg{X: dx, Y: dy, Button: tea.MouseLeft})
		require.Same(t, original, root.panes.root)
	}
}
func TestSelfSplitSkipsTabsAssignedToUnrelatedMultiPaneWorkspace(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.handleSwitchTab("third")
	before := root.panes.root
	_, _, ok := root.paneSplitCandidate(root.panes, "third", "third", splitRight)
	require.False(t, ok, "an unrelated saved split is not a pool of hidden replacement tabs")
	require.Same(t, before, root.panes.root)
}

func TestFocusedTabDragUsesOffscreenLeftNeighbor(t *testing.T) {
	root := splitTestRoot(t)
	root.handleSwitchTab("third")
	root.handleWindowResize(60, 40)
	root.tabBar.Update(messages.WheelCoalescedMsg{Delta: 1000})
	x, y := paneTabPoint(t, root, "third")
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Equal(t, []string{"profile", "second", "third"}, root.paneGesture.order)
	r := root.paneGesture.geometry.Panes["third"]
	dx, dy := edgePoint(r, splitBottom)
	root.Update(tea.MouseMotionMsg{X: dx, Y: dy, Button: tea.MouseLeft})
	root.Update(tea.MouseReleaseMsg{X: dx, Y: dy, Button: tea.MouseLeft})
	require.Equal(t, []string{"second", "third"}, root.panes.Sessions())
	require.Equal(t, "third", root.paneFocus())
}
