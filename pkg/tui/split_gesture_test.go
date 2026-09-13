package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func paneTabPoint(t *testing.T, root *appModel, id string) (int, int) {
	t.Helper()
	root.tabBar.View()
	y := root.contentHeight + 1
	for x := tabFrameOrigin(); x < root.width; x++ {
		if hit, ok := root.tabBar.TabBodyAt(x-tabFrameOrigin(), 0); ok && hit == id {
			return x, y
		}
	}
	t.Fatalf("tab %s not visible", id)
	return 0, 0
}

func TestSplitRootClickJitterAndCoalescedUpdrop(t *testing.T) {
	root := splitTestRoot(t)
	x, y := paneTabPoint(t, root, "second")
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Equal(t, "profile", root.paneFocus(), "press does not activate")
	root.Update(tea.MouseMotionMsg{X: x + 1, Y: y + 1, Button: tea.MouseLeft})
	require.False(t, root.paneGesture.active)
	root.Update(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Equal(t, "second", root.paneFocus())
	require.False(t, root.panesEnabled())
	root.handleSwitchTab("profile")
	x, y = paneTabPoint(t, root, "second")
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	_, bounds, _ := root.measurePanes()
	motion := tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y, Button: tea.MouseLeft}
	root.Update(messages.PointerBoundaryMsg{Pending: messages.PointerUpdateMsg{X: motion.X, Y: motion.Y, Motion: &motion}, Event: tea.MouseReleaseMsg{X: motion.X, Y: motion.Y, Button: tea.MouseLeft}})
	require.Equal(t, []string{"second", "profile"}, root.panes.Sessions(), "corner tie picks left; ordered motion must precede release")
	require.Equal(t, "second", root.paneFocus())
	require.Nil(t, root.paneGesture)
}

func TestSplitRootHorizontalReorderIsPreviewUntilRelease(t *testing.T) {
	root := splitTestRoot(t)
	x, y := paneTabPoint(t, root, "profile")
	dest, _ := paneTabPoint(t, root, "third")
	before := root.paneOrder()
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: dest + 12, Y: y, Button: tea.MouseLeft})
	require.Equal(t, before, root.paneOrder())
	require.True(t, root.paneGesture.reorder)
	_, cmd := root.Update(tea.MouseReleaseMsg{X: dest + 12, Y: y, Button: tea.MouseLeft})
	count := 0
	for _, msg := range collectMsgs(cmd) {
		if reorder, ok := msg.(messages.ReorderTabMsg); ok {
			count++
			root.Update(reorder)
		}
	}
	require.Equal(t, 1, count)
	require.NotEqual(t, before, root.paneOrder())
	require.False(t, root.panesEnabled())
}

func TestSplitRootCanceledTransactionsNeverChangeLayoutOrOrder(t *testing.T) {
	for _, cancel := range []string{"escape", "outside", "blur", "modal", "resize", "topology", "source-close"} {
		t.Run(cancel, func(t *testing.T) {
			root := splitTestRoot(t)
			x, y := paneTabPoint(t, root, "second")
			root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			before, order := root.panes.root, root.paneOrder()
			_, bounds, _ := root.measurePanes()
			root.Update(tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y + 2, Button: tea.MouseLeft})
			require.NotNil(t, root.paneGesture)
			require.NotNil(t, root.paneGestureLayer())
			switch cancel {
			case "escape":
				root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			case "outside":
				root.Update(tea.MouseReleaseMsg{X: -1, Y: -1, Button: tea.MouseLeft})
			case "blur":
				root.Update(tea.BlurMsg{})
			case "modal":
				root.Update(dialog.OpenDialogMsg{Model: &stubDialog{id: "modal"}})
				require.Nil(t, root.paneGestureLayer())
			case "resize":
				root.Update(tea.WindowSizeMsg{Width: 121, Height: 40})
			case "topology":
				root.panes = newSplitLayout("profile")
				before = root.panes.root
				root.Update(tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y + 3, Button: tea.MouseLeft})
			case "source-close":
				root.supervisor.CloseSession("second")
				order = root.paneOrder()
				root.Update(tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y + 3, Button: tea.MouseLeft})
			}
			require.Nil(t, root.paneGesture)
			require.Same(t, before, root.panes.root)
			require.Equal(t, order, root.paneOrder())
			require.Equal(t, "profile", root.paneFocus())
		})
	}
}

func TestSplitReleaseRehitTestsInvalidCenterSelfAndComposer(t *testing.T) {
	root := splitTestRoot(t)
	_, bounds, _ := root.measurePanes()
	for _, point := range [][2]int{{bounds.X + bounds.W/2, bounds.Y + bounds.H/2}, {bounds.X, root.height - 2}, {-1, bounds.Y}} {
		x, y := paneTabPoint(t, root, "second")
		root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		root.Update(tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y, Button: tea.MouseLeft})
		root.Update(tea.MouseReleaseMsg{X: point[0], Y: point[1], Button: tea.MouseLeft})
		require.False(t, root.panesEnabled())
		require.Equal(t, "profile", root.paneFocus())
	}
	x, y := paneTabPoint(t, root, "profile")
	root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: bounds.X, Y: bounds.Y, Button: tea.MouseLeft})
	root.Update(tea.MouseReleaseMsg{X: bounds.X, Y: bounds.Y, Button: tea.MouseLeft})
	require.False(t, root.panesEnabled(), "self drop is invalid")
}

func TestSplitDividerTransactionalKeyboardMouseAndNoMarkdownPreview(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	d := root.paneGeometry.Dividers[0]
	before := root.panes.root
	stats := root.chatPage.(resizeCacheReporter)
	a, b, c := stats.ResizeCacheStats()
	root.Update(tea.MouseClickMsg{X: d.Rect.X, Y: d.Rect.Y + 2, Button: tea.MouseLeft})
	root.Update(tea.MouseMotionMsg{X: d.Rect.X + 10, Y: d.Rect.Y + 2, Button: tea.MouseLeft})
	require.Same(t, before, root.panes.root)
	afterA, afterB, afterC := stats.ResizeCacheStats()
	require.Equal(t, [3]uint64{a, b, c}, [3]uint64{afterA, afterB, afterC}, "preview cannot reflow history")
	root.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Same(t, before, root.panes.root)
	root.handlePaneAction(messages.PaneActionMsg{Action: "resize", DividerID: d.ID})
	root.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotSame(t, before, root.panes.root)
	require.Equal(t, d.Rect.X+1, root.paneGeometry.Dividers[0].Rect.X)
	for _, r := range root.paneGeometry.Panes {
		require.GreaterOrEqual(t, r.W, paneMinWidth)
		require.GreaterOrEqual(t, r.H, paneMinHeight)
	}
}

func TestSplitDividerPreviewReturnsToOriginalPosition(t *testing.T) {
	for _, keyboard := range []bool{false, true} {
		t.Run(map[bool]string{false: "mouse", true: "keyboard"}[keyboard], func(t *testing.T) {
			root := splitTestRoot(t)
			root.splitPane("second", "profile", splitRight)
			d := root.paneGeometry.Dividers[0]
			original := root.panes.root
			if keyboard {
				root.handlePaneAction(messages.PaneActionMsg{Action: "resize", DividerID: d.ID})
				root.Update(tea.KeyPressMsg{Code: tea.KeyRight})
				root.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
			} else {
				root.Update(tea.MouseClickMsg{X: d.Rect.X, Y: d.Rect.Y + 2, Button: tea.MouseLeft})
				root.Update(tea.MouseMotionMsg{X: d.Rect.X + 7, Y: d.Rect.Y + 2, Button: tea.MouseLeft})
				root.Update(tea.MouseMotionMsg{X: d.Rect.X, Y: d.Rect.Y + 2, Button: tea.MouseLeft})
			}
			require.Equal(t, d.Rect, root.paneGesture.preview)
			require.Equal(t, dividerPosition(d), root.paneGesture.position)
			if keyboard {
				root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			} else {
				root.Update(tea.MouseReleaseMsg{X: d.Rect.X, Y: d.Rect.Y + 2, Button: tea.MouseLeft})
			}
			require.Nil(t, root.paneGesture)
			require.Same(t, original, root.panes.root, "return-to-origin commit is a true no-op")
			require.Equal(t, d.Rect, root.paneGeometry.Dividers[0].Rect)
		})
	}
}
