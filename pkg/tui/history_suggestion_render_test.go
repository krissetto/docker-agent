package tui

import (
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestHistorySuggestionRootKeepsCursorAndGhostColors(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, name := range []string{"black-cyan", "black-amber", "black-violet", "white-cobalt"} {
		t.Run(name, func(t *testing.T) {
			theme, err := styles.LoadTheme(name)
			require.NoError(t, err)
			styles.ApplyTheme(theme)
			root, _, _ := frozenClockRoot(t, 120, 40)
			root.history.Messages = []string{"some notes on the file browser and shell output"}
			root.focusedPanel = PanelEditor
			root.editor.Focus()
			root.editor.SetValue("some notes on the file brows")

			for _, char := range "er" {
				root.Update(tea.KeyPressMsg{Code: char, Text: string(char)})
				frame := root.View().Content
				if dir := os.Getenv("DOCKER_AGENT_TUI_FIXTURE_DIR"); dir != "" {
					require.NoError(t, os.WriteFile(filepath.Join(dir, "history-"+name+"-"+string(char)+".ansi.txt"), []byte(frame), 0o600))
				}
				require.Equal(t, frame, root.View().Content, "cached root frame preserves the cursor")
				prefix := root.editor.Value()
				found := false
				for row := range strings.SplitSeq(frame, "\n") {
					before, _, ok := strings.Cut(ansi.Strip(row), prefix)
					if !ok {
						continue
					}
					found = true
					cells := chromeCells(row)
					cursor := ansi.StringWidth(before + prefix)
					require.Greater(t, len(cells), cursor+2)
					require.Equal(t, color.NRGBAModel.Convert(styles.SuggestionCursorStyle.GetBackground()), color.NRGBAModel.Convert(cells[cursor].bg))
					require.Equal(t, color.NRGBAModel.Convert(styles.SuggestionCursorStyle.GetForeground()), color.NRGBAModel.Convert(cells[cursor].fg))
					for _, cell := range cells[cursor+1 : cursor+3] {
						require.Equal(t, color.NRGBAModel.Convert(styles.SuggestionGhostStyle.GetForeground()), color.NRGBAModel.Convert(cell.fg))
						require.Equal(t, color.NRGBAModel.Convert(styles.EditorBg), color.NRGBAModel.Convert(cell.bg))
					}
					break
				}
				require.True(t, found, "typed input appears in the composed root frame")
			}
		})
	}
}
