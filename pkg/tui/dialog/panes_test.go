package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/commands"
)

type paneChoiceTestMsg string

func paneTestItem(label, identity string, calls *int) commands.Item {
	return commands.Item{
		ID: identity, Label: label,
		Execute: func(argument string) tea.Cmd {
			(*calls)++
			return func() tea.Msg { return paneChoiceTestMsg(identity + argument) }
		},
	}
}

func TestPanesDialogCompactIdentityAndKeyboardExactOnce(t *testing.T) {
	calls := 0
	colored := "\x1b[38;2;12;140;200mreviewer\x1b[0m"
	items := []commands.Item{
		paneTestItem("Split right", "opaque-first", &calls),
		paneTestItem(colored+" · Review patch", "opaque-second", &calls),
	}
	items[1].Description = "node-2"
	d := NewPanesDialog("", items).(*panesDialog)
	d.SetSize(100, 35)
	view := d.View()
	plain := ansi.Strip(view)
	assert.Contains(t, plain, "Panes")
	assert.Contains(t, plain, "reviewer · Review patch · node-2")
	assert.Contains(t, view, colored, "root-supplied canonical agent color is preserved")
	assert.NotContains(t, plain, "opaque-")
	assert.NotContains(t, plain, "Commands")
	assert.Empty(t, d.actions, "choices are not an action-button wall")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	assert.Nil(t, cmd)
	assert.Zero(t, calls)
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	msgs := collectMsgs(cmd)
	require.Len(t, msgs, 2)
	assert.IsType(t, CloseDialogMsg{}, msgs[0])
	assert.Equal(t, paneChoiceTestMsg("opaque-second"), msgs[1])
	assert.Equal(t, 1, calls)
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd)
	assert.Nil(t, d.CancelDialogCmd())
	assert.Equal(t, 1, calls)
}

func TestPanesDialogFilterOwnsPrintableKeysAndPreservesRouting(t *testing.T) {
	calls := 0
	d := NewPanesDialog("Panes · choose session", []commands.Item{
		paneTestItem("Other", "jkq-hidden-id", &calls),
		paneTestItem("jkq agent · Task", "target", &calls),
	}).(*panesDialog)
	d.SetSize(100, 35)
	for _, code := range "jkq" {
		_, _ = d.Update(tea.KeyPressMsg{Code: code, Text: string(code)})
	}
	assert.Equal(t, "jkq", d.textInput.Value())
	require.Len(t, d.filtered, 1, "opaque IDs are not searchable presentation text")
	assert.Equal(t, "target", d.filtered[0].ID)
	assert.Contains(t, ansi.Strip(d.View()), "Panes · choose session")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	result, ok := findMsg[paneChoiceTestMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, paneChoiceTestMsg("target"), result)
}

func TestPanesDialogMouseSelectionAndDoubleClickApply(t *testing.T) {
	calls := 0
	d := NewPanesDialog("Panes", []commands.Item{
		paneTestItem("First", "first", &calls),
		paneTestItem("Second", "second", &calls),
	}).(*panesDialog)
	d.SetSize(80, 25)
	x, y, _, _ := d.BodyScrollBounds()
	click := tea.MouseClickMsg{X: x + 3, Y: y + 1, Button: tea.MouseLeft}
	_, cmd := d.Update(click)
	assert.Nil(t, cmd)
	assert.Equal(t, 1, d.selected)
	assert.Zero(t, calls)
	_, cmd = d.Update(click)
	result, ok := findMsg[paneChoiceTestMsg](collectMsgs(cmd))
	require.True(t, ok)
	assert.Equal(t, paneChoiceTestMsg("second"), result)
	_, cmd = d.Update(click)
	assert.Nil(t, cmd)
	assert.Equal(t, 1, calls)
}

func TestPanesDialogCancelNeverRunsAction(t *testing.T) {
	for _, route := range []string{"escape", "close", "manager"} {
		t.Run(route, func(t *testing.T) {
			calls := 0
			d := NewPanesDialog("Panes", []commands.Item{paneTestItem("Apply", "route", &calls)}).(*panesDialog)
			d.SetSize(80, 25)
			var cmd tea.Cmd
			switch route {
			case "escape":
				_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			case "close":
				view := d.View()
				row, col := d.Position()
				x, y, ok := closeControlCell(lipgloss.Width(view), lipgloss.Height(view))
				require.True(t, ok)
				_, cmd = d.Update(tea.MouseClickMsg{X: col + x, Y: row + y, Button: tea.MouseLeft})
			default:
				cmd = d.CancelDialogCmd()
			}
			msgs := collectMsgs(cmd)
			require.Len(t, msgs, 1)
			assert.IsType(t, CloseDialogMsg{}, msgs[0])
			assert.Nil(t, d.CancelDialogCmd())
			_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			assert.Nil(t, cmd)
			assert.Zero(t, calls)
		})
	}
}

func TestPanesDialogTinyBoundsAndSelectionVisibility(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {8, 4}, {20, 6}, {32, 10}, {80, 24}} {
		calls := 0
		items := make([]commands.Item, 12)
		for i := range items {
			items[i] = paneTestItem(string(rune('A'+i))+" · "+strings.Repeat("task ", 12), string(rune('a'+i)), &calls)
		}
		d := NewPanesDialog("Panes · choose divider", items).(*panesDialog)
		d.SetSize(size[0], size[1])
		for range items {
			view := d.View()
			assert.LessOrEqual(t, lipgloss.Width(view), size[0])
			assert.LessOrEqual(t, lipgloss.Height(view), size[1])
			if size[0] >= 20 && size[1] >= 6 {
				assert.Contains(t, ansi.Strip(view), "› "+string(rune('A'+d.selected)))
			}
			_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			assert.Nil(t, cmd)
		}
		assert.Zero(t, calls)
	}
}

func TestPanesDialogEmptyFilterCannotApply(t *testing.T) {
	calls := 0
	d := NewPanesDialog("Panes", []commands.Item{paneTestItem("Split", "identity", &calls)}).(*panesDialog)
	d.SetSize(80, 24)
	_, _ = d.Update(tea.PasteMsg{Content: "no matching entry"})
	assert.Empty(t, d.filtered)
	assert.Contains(t, ansi.Strip(d.View()), "No matching panes")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Nil(t, cmd)
	assert.False(t, d.responseSent)
	assert.Zero(t, calls)
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.True(t, hasMsg[CloseDialogMsg](collectMsgs(cmd)))
}
