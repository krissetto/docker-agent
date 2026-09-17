package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestScreenViewportRetainsRealEditorCursorAcrossResize(t *testing.T) {
	s := NewScreen("fixture", "branch", "type here")
	s.Status.Agent = "reviewer"
	s.Transcript.AddAssistant(strings.Repeat("historical transcript\n", 40))
	draft := strings.Repeat("wrapped 界 e\u0301 text\n", 30) + "END"
	s.Editor.SetText(draft)
	for _, position := range []int{0, len([]rune(draft)) / 2, len([]rune(draft))} {
		s.Editor.cursor = position
		for _, size := range [][2]int{{80, 24}, {20, 8}, {2, 2}, {1, 1}, {80, 24}} {
			lines, row, col := s.Frame(size[0], size[1], 0, false, nil, nil)
			require.GreaterOrEqual(t, row, max(0, len(lines)-size[1]), "renderer need not clamp cursor to a different row")
			require.Less(t, row, len(lines))
			require.GreaterOrEqual(t, col, 0)
			require.Less(t, col, size[0])
			for _, line := range lines[max(0, len(lines)-size[1]):] {
				require.LessOrEqual(t, DisplayWidth(line), size[0])
			}
			require.Equal(t, draft, s.Editor.Text())
			require.Equal(t, position, s.Editor.cursor)
			require.Equal(t, 1, s.Transcript.BlockCount(), "viewport does not rewrite transcript history")
		}
	}
}

func TestScreenAutocompleteAndDormancyUseBoundedExistingSeams(t *testing.T) {
	s := NewScreen("fixture", "", "type here")
	var commands []Command
	for i := range 20 {
		commands = append(commands, Command{Name: fmt.Sprintf("item%02d", i), Desc: strings.Repeat("long description 界\n", 20)})
	}
	s.Autocomplete.SetCommands(commands)
	s.Editor.SetText("/")
	s.Autocomplete.Sync("/")
	for range 15 {
		s.Autocomplete.MoveDown()
	}
	for _, height := range []int{24, 8, 3, 1, 24} {
		lines, row, col := s.Frame(40, height, 0, false, nil, nil)
		require.LessOrEqual(t, len(lines), height)
		require.GreaterOrEqual(t, row, 0)
		require.Less(t, row, len(lines))
		require.Less(t, col, 40)
		for _, line := range lines {
			require.LessOrEqual(t, DisplayWidth(line), 40)
		}
		if height >= 8 {
			require.Contains(t, ansi.Strip(strings.Join(lines, "\n")), "item15")
		}
	}
	s.Autocomplete.Dismiss()
	s.Status.Agent, s.Status.Dormant, s.Status.Pending = "reviewer", true, 2
	lines, _, _ := s.Frame(120, 24, 0, false, nil, nil)
	plain := ansi.Strip(strings.Join(lines, "\n"))
	require.Contains(t, plain, "Restored · paused · /resume")
	require.Contains(t, plain, "2 accepted inputs queued")
	require.Contains(t, plain, "Any queued work or reports will wait until you resume this session.")
	require.Equal(t, 1, strings.Count(plain, "reviewer"), "identity belongs to footer, not a replacement pane header")
}
