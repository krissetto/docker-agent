package editor

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestSuggestionStaysInsideTextareaAndTracksScrolledCursor(t *testing.T) {
	for _, width := range []int{1, 2, 12, 40} {
		for _, height := range []int{1, 2, 5} {
			e := New(nil).(*editor)
			e.SetSize(width, height)
			e.SetValue("first wrapped words here\nsecond line\nlast")
			e.textarea.MoveToEnd()
			draft, row, col := e.Value(), e.textarea.Line(), e.textarea.Column()
			e.hasSuggestion = true
			e.suggestion = strings.Repeat(" suggestion 界 e\u0301\n", 40)
			base := e.textarea.View()
			view := e.applySuggestionOverlay(base)
			require.Equal(t, lipgloss.Height(base), lipgloss.Height(view))
			for line := range strings.SplitSeq(view, "\n") {
				require.LessOrEqual(t, ansi.StringWidth(line), width)
			}
			require.Equal(t, draft, e.Value())
			require.Equal(t, row, e.textarea.Line())
			require.Equal(t, col, e.textarea.Column())
		}
	}
}

func TestViewportEditorKeepsCursorAndDraftAcrossTinyResize(t *testing.T) {
	e := New(nil).(*editor)
	e.SetValue("first 界 e\u0301 line\nsecond 界 line\nEND")
	e.textarea.MoveToEnd()
	draft, row, col := e.Value(), e.textarea.Line(), e.textarea.Column()
	for _, size := range [][2]int{{80, 8}, {16, 5}, {2, 2}, {1, 1}, {40, 8}} {
		e.SetViewportSize(size[0], size[1])
		e.SetSize(size[0], size[1])
		width, height := e.GetSize()
		require.LessOrEqual(t, width, size[0])
		require.LessOrEqual(t, height, size[1])
		require.Equal(t, width, lipgloss.Width(e.View()))
		require.Equal(t, height, lipgloss.Height(e.View()))
		require.Equal(t, draft, e.Value())
		require.Equal(t, row, e.textarea.Line())
		require.Equal(t, col, e.textarea.Column())
		info := e.textarea.LineInfo()
		cursorRow := info.RowOffset - e.textarea.ScrollYOffset()
		for i, line := range strings.Split(draft, "\n") {
			if i >= row {
				break
			}
			cursorRow += wrappedLineCount([]rune(line), e.textarea.Width())
		}
		require.GreaterOrEqual(t, cursorRow, 0)
		require.Less(t, cursorRow, e.textarea.Height(), "real cursor row is inside the textarea, not hidden by output clipping")
		probe := e.textarea
		probe.SetVirtualCursor(false)
		cursor := probe.Cursor()
		probe.SetVirtualCursor(true)
		require.NotNil(t, cursor)
		require.Equal(t, cursorRow, cursor.Y, "independent public cursor API agrees with the row count")
		require.GreaterOrEqual(t, cursor.Y, 0)
		require.Less(t, cursor.Y, e.textarea.Height())
		require.GreaterOrEqual(t, cursor.X, 0)
		require.Less(t, cursor.X, e.textarea.Width())
	}
}

func TestViewportResizePreservesTextareaSelection(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(40, 3)
	e.SetValue("first line\nsecond 界 exact selection")
	e.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	e.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	selected := e.textarea.SelectedText()
	require.NotEmpty(t, selected)
	draft, row, col := e.Value(), e.textarea.Line(), e.textarea.Column()
	for _, size := range [][2]int{{2, 2}, {1, 1}, {40, 8}} {
		e.SetViewportSize(size[0], size[1])
		e.SetSize(size[0], size[1])
		require.Equal(t, selected, e.textarea.SelectedText())
		require.Equal(t, draft, e.Value())
		require.Equal(t, row, e.textarea.Line())
		require.Equal(t, col, e.textarea.Column())
	}
}

func TestSuggestionAndOccupancyViewsPreserveEditingGeometry(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(12, 2)
	e.SetValue("first 界 line\nsecond e\u0301 line\nlast 👩‍💻")
	e.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	e.hasSuggestion, e.suggestion = true, " ghost\nmore"
	before, selected, value := e.textarea.Layout(), e.textarea.SelectedText(), e.Value()
	for range 4 {
		_ = e.View()
		_ = e.FreshView()
		_ = e.OccupiedTextCells()
		require.Equal(t, before, e.textarea.Layout())
		require.Equal(t, selected, e.textarea.SelectedText())
		require.Equal(t, value, e.Value())
	}
}
