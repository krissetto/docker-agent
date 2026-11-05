package shell

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestShellBodyThumbPressDragAndRelease(t *testing.T) {
	view := newScrollableShell(t, 60)
	_ = view.View()
	bodyRows := len(view.detailsLines())
	first, _ := shellBodyOutputRows(bodyRows)
	gutter := shellCardBodyLeft() + cardInnerWidth(60) - 1
	left := tea.Mouse{Button: tea.MouseLeft}

	handled, _ := view.MouseAt(first, shellCardBodyLeft(), tea.MouseClickMsg(left))
	require.False(t, handled, "a press on body text is not a scrollbar press")
	handled, _ = view.MouseAt(0, gutter, tea.MouseClickMsg(left))
	require.False(t, handled, "a press on the header row is not a body press")
	handled, _ = view.MouseAt(first+2, gutter, tea.MouseMotionMsg(left))
	require.False(t, handled, "motion without a drag is left to hover")
	handled, _ = view.MouseAt(first, gutter, tea.MouseReleaseMsg(left))
	require.False(t, handled, "release without a drag is not captured")

	handled, _ = view.MouseAt(first, gutter, tea.MouseClickMsg(left))
	require.True(t, handled, "pressing the thumb starts a drag")
	require.True(t, view.IsBodyDragging())
	require.Zero(t, view.bodyOffset)

	handled, _ = view.MouseAt(first+3, gutter, tea.MouseMotionMsg(left))
	require.True(t, handled)
	require.Positive(t, view.bodyOffset, "dragging the thumb scrolls the body")
	dragged := view.bodyOffset
	require.Equal(t, dragged, view.bodyScroll.ScrollOffset())
	require.Contains(t, ansi.Strip(view.View()), "more lines", "the rendered window follows the drag")
	require.Equal(t, dragged, view.bodyOffset, "rendering keeps the dragged offset")

	handled, _ = view.MouseAt(200, -40, tea.MouseMotionMsg(left))
	require.True(t, handled, "a drag keeps capture outside the body")
	require.Equal(t, shellBodyMaxOffset(bodyRows), view.bodyOffset)

	handled, _ = view.MouseAt(-5, 200, tea.MouseReleaseMsg(left))
	require.True(t, handled, "release outside the body still ends the drag")
	require.False(t, view.IsBodyDragging())
	handled, _ = view.MouseAt(first+1, gutter, tea.MouseMotionMsg(left))
	require.False(t, handled, "capture ends with the release")
	require.Equal(t, shellBodyMaxOffset(bodyRows), view.bodyOffset)

	view.SetExpanded(false)
	handled, _ = view.MouseAt(first, gutter, tea.MouseClickMsg(left))
	require.False(t, handled, "a collapsed card never captures the pointer")
}
