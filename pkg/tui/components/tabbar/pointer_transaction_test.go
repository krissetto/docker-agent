package tabbar

import (
	"testing"

	tea "charm.land/bubbletea/v2"
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
