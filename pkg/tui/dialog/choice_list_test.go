package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestChoiceListVerticalReadableRows(t *testing.T) {
	var b BaseDialog
	choices := []Action{
		{Label: "No", Key: tea.KeyPressMsg{Code: 'N', Text: "N"}, Default: true, HideShortcut: true},
		{Label: "Always allow tool", Key: tea.KeyPressMsg{Code: 'T', Text: "T"}},
		{Label: "Reject with reason", Key: tea.KeyPressMsg{Code: 'R', Text: "R"}},
	}
	view := b.RenderChoices(40, choices...)
	lines := strings.Split(ansi.Strip(view), "\n")
	require.Len(t, lines, 3, "each decision occupies its own full-width row")
	assert.Equal(t, "› No ↵", strings.TrimSpace(lines[0]))
	assert.Equal(t, "Always allow tool T", strings.TrimSpace(lines[1]))
	assert.Equal(t, "Reject with reason R", strings.TrimSpace(lines[2]))
	for _, line := range lines {
		assert.Equal(t, 40, ansi.StringWidth(line))
	}
	assertToolActionsNotUnderlined(t, view)
	assert.True(t, b.FocusDefaultAction())
	_, handled := b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.True(t, handled)
	key, ok := b.SelectedActionKey()
	require.True(t, ok)
	assert.Equal(t, 'T', key.Code)
	view = b.RenderChoices(40, choices...)
	assert.Contains(t, ansi.Strip(view), "› Always allow tool ↵ T")
}

func TestChoiceListWrapsWithoutLosingLabelsOrHitTargets(t *testing.T) {
	for _, width := range []int{1, 2, 8, 16, 40} {
		var b BaseDialog
		label := "Always allow tool"
		view := b.RenderChoices(width,
			Action{Label: label, Key: tea.KeyPressMsg{Code: 'T', Text: "T"}},
			Action{Label: "Disabled", Key: tea.KeyPressMsg{Code: 'D'}, Disabled: true},
		)
		assert.LessOrEqual(t, lipgloss.Width(view), width)
		var text strings.Builder
		for _, row := range b.actionRows[:b.actionLines[1]] {
			text.WriteString(strings.TrimSpace(row.text))
			require.Len(t, row.hits, 1)
			assert.Equal(t, 'T', row.hits[0].key.Code)
			assert.Equal(t, width, row.hits[0].width)
		}
		assert.Equal(t, strings.ReplaceAll(label+"T", " ", ""), strings.ReplaceAll(text.String(), " ", ""))
		for _, row := range b.actionRows[b.actionLines[1]:] {
			assert.Empty(t, row.hits, "disabled choices cannot activate")
		}
	}
}

func TestChoiceListUsesBoundedViewportAndRevealsKeyboardSelection(t *testing.T) {
	var b BaseDialog
	b.SetSize(30, 6)
	choices := make([]Action, 8)
	for i := range choices {
		code := rune('a' + i)
		choices[i] = Action{Label: "Choice " + string(code), Key: tea.KeyPressMsg{Code: code, Text: string(code)}, Default: i == 0}
	}
	render := func() string {
		footer := b.RenderChoices(24, choices...)
		b.PrepareScrollableBody(styles.DialogStyle, 30, "Choose", "Details", footer)
		return b.RenderScrollableBody(styles.DialogStyle, 30, "Choose", "Details", footer)
	}
	_ = render()
	require.True(t, b.actionScrollActive)
	require.True(t, b.FocusDefaultAction())
	for i := range choices {
		view := render()
		assert.LessOrEqual(t, lipgloss.Width(view), 30)
		assert.LessOrEqual(t, lipgloss.Height(view), 6)
		assert.Contains(t, ansi.Strip(view), "Choice "+string(rune('a'+i)))
		row, col := b.CenterDialog(view)
		dl := NewDialogLayout(view, row, col)
		found := false
		for y := row; y < row+lipgloss.Height(view); y++ {
			for x := col; x < col+lipgloss.Width(view); x++ {
				if key, hit := b.ActionKeyAt(x, y, dl); hit && key.Code == rune('a'+i) {
					found = true
				}
			}
		}
		assert.True(t, found, "selected row must be mouse reachable")
		key, handled := b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyDown})
		assert.True(t, handled)
		assert.Zero(t, key.Code, "navigation never dispatches a choice")
	}
}
