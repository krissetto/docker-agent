package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestCloseWorkspaceConfirmationKeyboardAndSingleResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keys    []tea.KeyPressMsg
		confirm bool
	}{
		{"escape", []tea.KeyPressMsg{{Code: tea.KeyEscape}}, false},
		{"default-enter", []tea.KeyPressMsg{{Code: tea.KeyEnter}}, false},
		{"no", []tea.KeyPressMsg{{Code: 'n', Text: "n"}}, false},
		{"yes", []tea.KeyPressMsg{{Code: 'y', Text: "y"}}, true},
		{"navigate", []tea.KeyPressMsg{{Code: tea.KeyRight}, {Code: tea.KeyEnter}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			d := NewCloseWorkspaceDialog(3, func() tea.Msg { calls++; return nil })
			d.SetSize(100, 30)
			view := ansi.Strip(d.View())
			require.Contains(t, view, "Close workspace?")
			require.Contains(t, view, "all 3 panes")
			require.Contains(t, view, "keep running")
			require.Contains(t, view, "Cancel ↵")
			require.NotContains(t, view, "Remember")
			for _, key := range tc.keys {
				_, cmd := d.Update(key)
				collectMsgs(cmd)
			}
			want := 0
			if tc.confirm {
				want = 1
			}
			require.Equal(t, want, calls)
			_, cmd := d.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
			require.Nil(t, cmd)
		})
	}
}

func TestCloseWorkspaceConfirmationMeasuredMouseActions(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {40, 12}, {24, 8}} {
		for _, code := range []rune{'n', 'y'} {
			calls := 0
			d := NewCloseWorkspaceDialog(2, func() tea.Msg { calls++; return nil }).(*removalConfirmationDialog)
			d.SetSize(size[0], size[1])
			view := d.View()
			row, col := d.Position()
			dl := NewDialogLayout(view, row, col)
			clicked := false
			for y := row; y < row+dl.Height && !clicked; y++ {
				for x := col; x < col+dl.Width; x++ {
					key, hit := d.ActionKeyAt(x, y, dl)
					if hit && key.Code == code {
						_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
						collectMsgs(cmd)
						clicked = true
						break
					}
				}
			}
			require.True(t, clicked)
			if code == 'y' {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
		}
	}
}
