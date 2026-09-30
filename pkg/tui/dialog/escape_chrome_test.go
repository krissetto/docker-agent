package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/help"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestEscapeShortcutHiddenAcrossActionPresentations(t *testing.T) {
	for _, presentation := range []string{"buttons", "picker", "choices"} {
		for _, width := range []int{16, 40, 120} {
			t.Run(presentation, func(t *testing.T) {
				b := BaseDialog{}
				b.SetSize(width+6, 30)
				actions := []Action{{Label: "Cancel", Key: tea.KeyPressMsg{Code: tea.KeyEscape}, Default: true}, {Label: "Apply", Key: tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}}}
				var footer string
				switch presentation {
				case "buttons":
					footer = b.RenderActions(width, actions...)
				case "picker":
					footer = b.RenderPickerFooter(width, actions...)
				case "choices":
					footer = b.RenderChoices(width, actions...)
				}
				require.NotRegexp(t, `(?i)\besc(?:ape)?\b`, ansi.Strip(footer))
				require.Contains(t, ansi.Strip(footer), "Cancel")
				require.Contains(t, ansi.Strip(footer), "ctrl+s", "other documented action shortcuts are unchanged")
				b.PrepareScrollableBody(styles.DialogStyle, width+6, "Title", "body", footer)
				view := b.RenderScrollableBody(styles.DialogStyle, width+6, "Title", "body", footer)
				row, col := b.CenterDialog(view)
				dl := NewDialogLayout(view, row, col)
				cancelCells := 0
				for y := row; y < row+dl.Height; y++ {
					for x := col; x < col+dl.Width; x++ {
						if key, ok := b.ActionKeyAt(x, y, dl); ok && key.Code == tea.KeyEscape {
							cancelCells++
						}
					}
				}
				require.Positive(t, cancelCells, "Cancel remains clickable without its shortcut badge")
				require.True(t, b.FocusActions(false))
				key, handled := b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyEnter})
				require.True(t, handled)
				require.Equal(t, tea.KeyEscape, key.Code, "focused Cancel keeps its original semantic key")
			})
		}
	}
}

func TestHelpRetainsExplicitEscapeReferenceWithoutRoutineDismissHint(t *testing.T) {
	d := NewHelpDialog(help.Document{Current: []help.Section{{Title: "Keyboard reference", Entries: []help.Entry{{ID: "cancel", Keys: []string{"esc"}, Description: "Cancel the current operation"}}}}}).(*helpDialog)
	d.SetSize(120, 40)
	view := ansi.Strip(d.View())
	require.Contains(t, view, "esc", "explicit shortcut documentation is not routine dialog chrome")
	require.NotContains(t, view, "enter/q/esc")
	require.NotContains(t, view, "Esc close")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	require.True(t, hasDialogMsg[CloseDialogMsg](collectMsgs(cmd)))
}
