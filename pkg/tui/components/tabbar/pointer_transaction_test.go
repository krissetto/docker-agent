package tabbar

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/stretchr/testify/require"
)

func TestPointerTransactionUsesCellGeometryAndCancelsWithoutCommands(t *testing.T) {
	tb := New(newMotionRuntime(), 12)
	tabs := motionTabs(3, 0)
	tabs[1].Title = "界éλ"
	tb.SetWidth(80)
	tb.SetTabs(tabs, 0)
	tb.View()
	bound := boundForSession(t, tb, "b")
	id, ok := tb.TabBodyAt(bound.start, 0)
	require.True(t, ok)
	require.Equal(t, "b", id)
	_, ok = tb.TabBodyAt(bound.start, 1)
	require.False(t, ok)
	require.Nil(t, tb.BeginPointerPreview(bound.start))
	require.True(t, tb.drag.pending)
	require.False(t, tb.drag.active)
	tb.PreviewPointer(bound.start + 12)
	require.True(t, tb.drag.active)
	tb.CancelPointer()
	require.False(t, tb.HasPointerCapture())
	require.False(t, tb.HasFloatingOverlay())
	require.Empty(t, commandMessages(tb.Update(tea.MouseReleaseMsg{X: bound.start + 12, Button: tea.MouseLeft})))
	require.Equal(t, tabs, tb.tabs)
}

func TestPointerBlurNeverCommitsReorder(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tabs := motionTabs(4, 0)
	tb.SetWidth(100)
	tb.SetTabs(tabs, 0)
	tb.View()
	x := boundForSession(t, tb, "a").start
	tb.BeginPointerPreview(x)
	tb.PreviewPointer(x + 45)
	require.True(t, tb.drag.active)
	require.Empty(t, commandMessages(tb.Update(tea.BlurMsg{})))
	require.Equal(t, tabs, tb.tabs)
	require.False(t, tb.HasFloatingOverlay())
	require.False(t, tb.HasPointerCapture())
}

func TestBackgroundDialogHoverDoesNotActivatePendingTabDrag(t *testing.T) {
	tb := New(newMotionRuntime(), 12)
	tb.SetWidth(80)
	tb.SetTabs(motionTabs(3, 0), 0)
	tb.View()
	x := boundForSession(t, tb, "b").start
	tb.BeginPointerPreview(x)
	tb.UpdateHover(x+12, 0)
	require.True(t, tb.drag.pending, "background hover preserves click release ownership")
	require.False(t, tb.drag.active, "background hover never activates reorder")
	require.Nil(t, tb.GetDragLayerInfo(80, 0), "no overlay behind dialog")
	tb.CancelPointer()
	require.False(t, tb.HasPointerCapture())
}

func TestPaneDragViewMatchesCanonicalFloatingTab(t *testing.T) {
	for _, width := range []int{16, 40, 120} {
		for _, active := range []bool{false, true} {
			ar := newMotionRuntime()
			tb := New(ar, 20)
			tb.SetWidth(width)
			tabs := []messages.TabInfo{{SessionID: "source", Title: "界 👩‍💻 long title", AgentName: "reviewer", IsActive: active, IsRunning: true, IsAttached: true}}
			tb.SetTabs(tabs, 0)
			tb.View()
			bound := boundForSession(t, tb, "source")
			x := min(bound.start+2, bound.end-1)
			offset := tb.DragGrabOffset("source", x)
			before := tb.DragTabView("source")
			tb.BeginPointerPreview(x)
			tb.PreviewPointer(x + 3)
			layer := tb.GetDragLayerInfo(width, 5)
			require.NotNil(t, layer)
			require.Equal(t, ansi.Truncate(tb.DragTabView("source"), width, ""), layer.Content, "same full styled cells, not a text-only approximation")
			require.Equal(t, offset, tb.drag.grabOffset)
			require.Equal(t, ansi.Strip(before), ansi.Strip(tb.DragTabView("source")))
			require.Contains(t, ansi.Strip(before), "×")
			tb.CancelPointer()
			require.Equal(t, layer.Content, ansi.Truncate(tb.DragTabView("source"), width, ""))
			ar.Stop()
		}
	}
}

func TestResumePaneDragKeepsIdentityAcrossScrolledStripAndStatusUpdate(t *testing.T) {
	ar := newMotionRuntime()
	defer ar.Stop()
	tb := New(ar, 18)
	tb.SetWidth(45)
	tabs := motionTabs(4, 0)
	tabs[0].AgentName = "reviewer"
	tabs[0].Title = "界 é 👩‍💻 source"
	tb.SetTabs(tabs, 0)
	tb.View()
	tb.ResumePointerPreview("a", 3, 8)
	before := tb.GetDragLayerInfo(45, 2)
	require.NotNil(t, before)
	tb.CancelPointer()
	tb.scrollOffset = 25
	tb.recordVisualState()
	tabs[0].NeedsAttention = true
	tabs[0].IsAttached = true
	tb.SetTabs(tabs, 0)
	outside := tb.DragTabView("a")
	tb.ResumePointerPreview("a", 3, 8)
	inside := tb.GetDragLayerInfo(45, 2)
	require.NotNil(t, inside)
	require.Equal(t, outside, inside.Content, "new canonical status has the same cells inside and outside")
	require.Equal(t, before.X, inside.X, "reentry must not recapture another clipped tab at the old press coordinate")
	require.Equal(t, 0, tb.drag.dragIdx)
	require.Equal(t, 3, tb.drag.grabOffset)
}
