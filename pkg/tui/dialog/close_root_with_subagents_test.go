package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func closeTabChoiceDialog(t *testing.T) *closeRootWithSubagentsDialog {
	t.Helper()
	d := NewCloseRootWithSubagentsDialog("root").(*closeRootWithSubagentsDialog)
	d.SetSize(100, 30)
	return d
}

func tickRememberChoice(t *testing.T, d *closeRootWithSubagentsDialog) {
	t.Helper()
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Nil(t, cmd)
	require.True(t, d.rememberFocused)
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	require.Nil(t, cmd)
	require.True(t, d.rememberChoice)
}

func TestCloseTabRememberChoiceKeyboardFocus(t *testing.T) {
	t.Parallel()
	d := closeTabChoiceDialog(t)
	assert.Contains(t, ansi.Strip(d.View()), "[ ] Remember this choice")
	assert.Contains(t, ansi.Strip(d.View()), "No ↵")
	tickRememberChoice(t, d)
	assert.Contains(t, ansi.Strip(d.View()), "[x] Remember this choice")
	assert.NotContains(t, ansi.Strip(d.View()), "No ↵", "checkbox Enter toggles, never chooses No")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, cmd)
	assert.False(t, d.rememberChoice)

	for _, want := range []int{0, 1} {
		_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		require.Nil(t, cmd)
		require.True(t, d.ActionsFocused())
		assert.Equal(t, want, d.focusedAction)
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	require.True(t, d.rememberFocused)
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.True(t, d.ActionsFocused())
	assert.Equal(t, 1, d.focusedAction)
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	assert.Equal(t, 0, d.focusedAction)
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	assert.True(t, d.rememberFocused)
}

func TestCloseTabRememberChoiceOnlyExplicitAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want tea.Msg
	}{
		{"yes", tea.KeyPressMsg{Code: 'y', Text: "y"}, CloseRootWithSubagentsConfirmedMsg{SessionID: "root", RememberChoice: true}},
		{"no", tea.KeyPressMsg{Code: 'n', Text: "n"}, RememberCloseTabNoMsg{}},
		{"default-no", tea.KeyPressMsg{Code: tea.KeyEnter}, RememberCloseTabNoMsg{}},
		{"escape", tea.KeyPressMsg{Code: tea.KeyEscape}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := closeTabChoiceDialog(t)
			tickRememberChoice(t, d)
			d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			_, cmd := d.Update(tc.key)
			msgs := collectMsgs(cmd)
			assert.Contains(t, msgs, CloseDialogMsg{})
			if tc.want != nil {
				assert.Contains(t, msgs, tc.want)
			} else {
				assert.Len(t, msgs, 1, "dismissal does not remember or close a tab")
			}
			_, cmd = d.Update(tc.key)
			assert.Nil(t, cmd, "answer can only be sent once")
		})
	}
}

func TestCloseTabRememberChoiceMouseAndDismiss(t *testing.T) {
	t.Parallel()
	for _, width := range []int{24, 40, 100} {
		d := closeTabChoiceDialog(t)
		d.SetSize(width, 20)
		d.EnsureBodyLineVisible(d.rememberLine)
		x, y, _, _ := d.BodyScrollBounds()
		y += d.rememberLine - d.BodyScrollOffset()
		require.True(t, d.rememberAt(x, y))
		_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
		require.Nil(t, cmd)
		require.True(t, d.rememberChoice)
		assert.False(t, d.rememberAt(x-1, y))
		assert.False(t, d.rememberAt(x, y-1))

		view := d.View()
		row, col := d.Position()
		cx, cy, ok := closeControlCell(lipgloss.Width(view), lipgloss.Height(view))
		require.True(t, ok)
		_, cmd = d.Update(tea.MouseClickMsg{X: col + cx, Y: row + cy, Button: tea.MouseLeft})
		assert.Equal(t, []tea.Msg{CloseDialogMsg{}}, collectMsgs(cmd))
	}
}

func TestCloseTabUncheckedNoDoesNotRemember(t *testing.T) {
	t.Parallel()
	for _, key := range []tea.KeyPressMsg{{Code: 'n', Text: "n"}, {Code: tea.KeyEnter}} {
		d := closeTabChoiceDialog(t)
		_, cmd := d.Update(key)
		assert.Equal(t, []tea.Msg{CloseDialogMsg{}}, collectMsgs(cmd))
	}
}
