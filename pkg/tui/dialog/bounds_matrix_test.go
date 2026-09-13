package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestTopLevelDialogRenderedBoundsMatrix(t *testing.T) {
	sizes := []struct{ w, h int }{{24, 6}, {40, 12}, {60, 20}, {120, 40}}
	factories := []struct {
		name string
		new  func() Dialog
	}{
		{"ctrl-k", func() Dialog { return NewCommandPaletteDialog([]commands.Category{{Name: "General"}}) }},
		{"settings", func() Dialog { return NewSettingsDialog(messages.Preferences{}, true) }},
	}
	for _, f := range factories {
		for _, size := range sizes {
			t.Run(f.name+"/"+string(rune(size.w)), func(t *testing.T) {
				d := f.new()
				d.SetSize(size.w, size.h)
				view := d.View()
				require.LessOrEqual(t, lipgloss.Width(view), size.w)
				require.LessOrEqual(t, lipgloss.Height(view), size.h)
				row, col := d.Position()
				require.GreaterOrEqual(t, row, 0)
				require.GreaterOrEqual(t, col, 0)
				require.LessOrEqual(t, col+lipgloss.Width(view), size.w)
				require.LessOrEqual(t, row+lipgloss.Height(view), size.h)
			})
		}
	}
}

func TestSettingsWideNarrowWideRetainsSelectionAndBounds(t *testing.T) {
	d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
	d.selected[d.tab] = d.rowCount() - 1
	selected := d.selected[d.tab]
	for _, size := range [][2]int{{120, 40}, {24, 6}, {120, 40}} {
		d.SetSize(size[0], size[1])
		view := d.View()
		require.LessOrEqual(t, lipgloss.Width(view), size[0])
		require.LessOrEqual(t, lipgloss.Height(view), size[1])
	}
	require.Equal(t, selected, d.selected[d.tab])
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	require.Less(t, d.selected[d.tab], selected)
}
