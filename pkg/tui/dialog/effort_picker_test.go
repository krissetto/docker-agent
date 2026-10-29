package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/effort"
)

func newTestEffortPicker(t *testing.T, levels []effort.Level, current effort.Level) *effortPickerDialog {
	t.Helper()
	dialog := NewEffortPickerDialog(levels, current)
	d, ok := dialog.(*effortPickerDialog)
	require.True(t, ok)
	d.Init()
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 50})
	return d
}

func TestEffortPickerPreselectsCurrent(t *testing.T) {
	t.Parallel()

	levels := []effort.Level{effort.None, effort.Low, effort.Medium, effort.High}
	d := newTestEffortPicker(t, levels, effort.Medium)

	require.Equal(t, 2, d.selected, "current level should be preselected")
}

func TestEffortPickerUnknownCurrentSelectsFirst(t *testing.T) {
	t.Parallel()

	levels := []effort.Level{effort.None, effort.Low, effort.Medium, effort.High}
	d := newTestEffortPicker(t, levels, "")

	require.Equal(t, 0, d.selected, "unknown current should leave the first level selected")
}

func TestEffortPickerNavigation(t *testing.T) {
	t.Parallel()

	levels := []effort.Level{effort.None, effort.Low, effort.Medium}
	d := newTestEffortPicker(t, levels, effort.None)

	downKey := tea.KeyPressMsg{Code: tea.KeyDown}
	upKey := tea.KeyPressMsg{Code: tea.KeyUp}

	d.Update(downKey)
	require.Equal(t, 1, d.selected)

	d.Update(downKey)
	require.Equal(t, 2, d.selected)

	// At the end, down must not move past the last level.
	d.Update(downKey)
	require.Equal(t, 2, d.selected)

	d.Update(upKey)
	require.Equal(t, 1, d.selected)

	d.Update(upKey)
	d.Update(upKey)
	require.Equal(t, 0, d.selected, "up must not move before the first level")
}

func TestEffortPickerSelectionReturnsCommand(t *testing.T) {
	t.Parallel()

	levels := []effort.Level{effort.None, effort.Low, effort.Medium}
	d := newTestEffortPicker(t, levels, effort.Low)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd, "enter should return a command")
}

func TestEffortPickerEscape(t *testing.T) {
	t.Parallel()

	levels := []effort.Level{effort.None, effort.Low}
	d := newTestEffortPicker(t, levels, effort.Low)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd, "escape should return a command")
}

func TestEffortPickerViewShowsLevelsAndCurrent(t *testing.T) {
	t.Parallel()

	levels := []effort.Level{effort.None, effort.Low, effort.Medium, effort.High, effort.Max}
	d := newTestEffortPicker(t, levels, effort.High)

	view := d.View()
	assert.Contains(t, view, "Select Reasoning Effort")
	for _, level := range levels {
		assert.Contains(t, view, level.String())
	}
	assert.Contains(t, view, "(current)")
}

func TestEffortPickerViewChangesOnNavigation(t *testing.T) {
	t.Parallel()

	levels := []effort.Level{effort.None, effort.Low, effort.Medium}
	d := newTestEffortPicker(t, levels, effort.None)

	view1 := d.View()
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	view2 := d.View()

	require.NotEqual(t, view1, view2, "view should change after navigation")
}

func TestEffortPickerKeyBindings(t *testing.T) {
	t.Parallel()

	levels := []effort.Level{effort.None, effort.Low, effort.Medium}
	tests := []struct {
		name  string
		key   tea.KeyPressMsg
		start int
		want  int
	}{
		{name: "down", key: tea.KeyPressMsg{Code: tea.KeyDown}, start: 0, want: 1},
		{name: "j", key: tea.KeyPressMsg{Code: 'j', Text: "j"}, start: 0, want: 1},
		{name: "ctrl+j", key: tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}, start: 0, want: 1},
		{name: "up", key: tea.KeyPressMsg{Code: tea.KeyUp}, start: 2, want: 1},
		{name: "k", key: tea.KeyPressMsg{Code: 'k', Text: "k"}, start: 2, want: 1},
		{name: "ctrl+k", key: tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}, start: 2, want: 1},
		{name: "end", key: tea.KeyPressMsg{Code: tea.KeyEnd}, start: 0, want: 2},
		{name: "G", key: tea.KeyPressMsg{Code: 'G', Text: "G"}, start: 0, want: 2},
		{name: "home", key: tea.KeyPressMsg{Code: tea.KeyHome}, start: 2, want: 0},
		{name: "g", key: tea.KeyPressMsg{Code: 'g', Text: "g"}, start: 2, want: 0},
		{name: "unbound", key: tea.KeyPressMsg{Code: 'x', Text: "x"}, start: 1, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newTestEffortPicker(t, levels, levels[tt.start])
			d.Update(tt.key)
			require.Equal(t, tt.want, d.selected)
		})
	}
}
