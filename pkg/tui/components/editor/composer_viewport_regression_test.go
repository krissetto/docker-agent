package editor

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func requireComposerCursorVisible(t *testing.T, e *editor) {
	t.Helper()
	// Public cursor coordinates, not a cursor move to make viewport repair pass.
	probe := e.textarea
	probe.SetVirtualCursor(false)
	cursor := probe.Cursor()
	probe.SetVirtualCursor(true)
	require.NotNil(t, cursor)
	require.GreaterOrEqual(t, cursor.Y, 0)
	require.Less(t, cursor.Y, e.textarea.Height())
}

func TestComposerViewportNewlineGrowthShowsFirstLineImmediately(t *testing.T) {
	for _, first := range []string{"FIRST", "界 cafe\u0301 👩‍💻"} {
		e := New(nil).(*editor)
		e.SetSize(48, 1)
		e.Update(tea.PasteMsg{Content: first})
		for rows := 2; rows <= 8; rows++ {
			e.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
			row, col, value := e.textarea.Line(), e.textarea.Column(), e.Value()
			e.SetSize(48, rows)
			// Inspect this frame before any subsequent key/textarea Update.
			require.Contains(t, ansi.Strip(e.View()), first)
			require.Zero(t, e.textarea.ScrollYOffset(), "all actual rows fit")
			require.Equal(t, value, e.Value())
			require.Equal(t, row, e.textarea.Line())
			require.Equal(t, col, e.textarea.Column())
			requireComposerCursorVisible(t, e)
			e.Update(tea.PasteMsg{Content: "next"})
		}
	}
}

func TestComposerViewportRewrapAndSameSizeShrinkAreImmediate(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(64, 4)
	e.SetValue("FIRST words 界 cafe\u0301 👩‍💻\nsecond words\nEND")
	draft, row, col := e.Value(), e.textarea.Line(), e.textarea.Column()
	for _, width := range []int{12, 64, 8, 64} {
		e.SetSize(width, 4)
		require.Equal(t, draft, e.Value())
		require.Equal(t, row, e.textarea.Line())
		require.Equal(t, col, e.textarea.Column())
		requireComposerCursorVisible(t, e)
		if width == 64 {
			require.Zero(t, e.textarea.ScrollYOffset())
			require.Contains(t, ansi.Strip(e.View()), "FIRST words 界 cafe\u0301 👩‍💻")
		}
	}
	// Actual select-all/paste changes content while the allocation stays fixed.
	e.SetSize(64, 1)
	e.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	e.Update(tea.PasteMsg{Content: "REPLACED"})
	e.SetSize(64, 1)
	require.Equal(t, "REPLACED", e.Value())
	require.Zero(t, e.textarea.ScrollYOffset())
	require.Contains(t, ansi.Strip(e.View()), "REPLACED")
	requireComposerCursorVisible(t, e)
}

func TestComposerViewportAdapterPreservesSelectionPolicyAndHistoryRow(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(50, 3)
	e.SetValue("FIRST\nselected 界 cafe\u0301 👩‍💻")
	e.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	e.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	selection := e.textarea.SelectedText()
	require.NotEmpty(t, selection)
	value, row, col := e.Value(), e.textarea.Line(), e.textarea.Column()
	dynamic, minH, maxH, maxContent := e.textarea.DynamicHeight(), e.textarea.MinHeight(), e.textarea.MaxHeight(), e.textarea.MaxContentHeight()
	for _, size := range [][2]int{{9, 1}, {50, 8}, {50, 8}} {
		e.SetSize(size[0], size[1])
		require.Equal(t, value, e.Value())
		require.Equal(t, row, e.textarea.Line())
		require.Equal(t, col, e.textarea.Column())
		require.Equal(t, selection, e.textarea.SelectedText())
		require.Equal(t, dynamic, e.textarea.DynamicHeight())
		require.Equal(t, minH, e.textarea.MinHeight())
		require.Equal(t, maxH, e.textarea.MaxHeight())
		require.Equal(t, maxContent, e.textarea.MaxContentHeight())
		requireComposerCursorVisible(t, e)
	}
	e.EnterHistorySearch()
	e.SetSize(50, 3)
	require.Equal(t, 2, e.textarea.Height(), "search owns one allocated row")
	e.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, value, e.Value())
	require.Equal(t, 3, e.textarea.Height())
	require.Zero(t, e.textarea.ScrollYOffset())
}

func TestComposerViewportAdapterRetainsLegacyLogicalLineLimit(t *testing.T) {
	e := New(nil).(*editor)
	e.SetSize(8, 1)
	require.Equal(t, 99, e.textarea.MaxHeight())
	for inserted := range 98 {
		e.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
		e.SetSize(8, 1)
		require.Equal(t, inserted+2, e.textarea.LineCount())
	}
	require.Equal(t, 99, e.textarea.LineCount())
	// Rejected newlines still pass through the real editor. They must not
	// fall through to submission/reset once the logical-line cap is reached.
	value, selection := e.Value(), e.textarea.SelectedText()
	dynamic, minH, maxH, maxContent := e.textarea.DynamicHeight(), e.textarea.MinHeight(), e.textarea.MaxHeight(), e.textarea.MaxContentHeight()
	for range 12 {
		_, cmd := e.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
		require.Nil(t, cmd, "a rejected newline must not emit a send command")
		e.SetSize(8, 1)
		require.Equal(t, value, e.Value())
		require.Equal(t, selection, e.textarea.SelectedText())
		require.Equal(t, 99, e.textarea.LineCount())
	}
	require.Equal(t, dynamic, e.textarea.DynamicHeight())
	require.Equal(t, minH, e.textarea.MinHeight())
	require.Equal(t, maxH, e.textarea.MaxHeight())
	require.Equal(t, maxContent, e.textarea.MaxContentHeight())
	require.Equal(t, 99, e.textarea.MaxHeight())
	require.Zero(t, e.textarea.MaxContentHeight())
	require.Positive(t, e.textarea.ScrollYOffset())
	requireComposerCursorVisible(t, e)
	_, send := e.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, send, "the configured send key still submits at the cap")
	require.Empty(t, e.Value())

	// Many visual rows must not consume the legacy logical-line budget.
	e.SetValue(strings.Repeat("wrapped ", 110))
	require.Greater(t, e.ContentLineCount(), 99)
	e.Update(tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl})
	require.Equal(t, 2, e.textarea.LineCount())
	e.SetSize(8, 105)
	require.Equal(t, 105, e.textarea.Height(), "allocation is not clamped to the input's 99-line policy")
	require.Equal(t, 99, e.textarea.MaxHeight())
}

func TestComposerViewportWidthMessageUsesOuterWidth(t *testing.T) {
	e := New(nil).(*editor)
	e.textarea.SetPrompt("> ")
	e.SetSize(30, 3)
	e.SetValue("FIRST\nEND")
	for range 3 {
		e.Update(tea.WindowSizeMsg{Width: 32, Height: 12})
		require.Equal(t, 30, e.width)
		require.Equal(t, 28, e.textarea.Width(), "prompt reserve is subtracted exactly once")
		e.SetSize(30, 3)
		require.Equal(t, 28, e.textarea.Width())
		require.Zero(t, e.textarea.ScrollYOffset())
		require.Contains(t, ansi.Strip(e.View()), "FIRST")
	}
}
