package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUIKeysPinnedKeySets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		binding []string
		want    string
	}{
		{"up", UIKeys.Up.Keys(), "up"},
		{"down", UIKeys.Down.Keys(), "down"},
		{"left", UIKeys.Left.Keys(), "left"},
		{"right", UIKeys.Right.Keys(), "right"},
		{"page up", UIKeys.PageUp.Keys(), "pgup"},
		{"page down", UIKeys.PageDown.Keys(), "pgdown"},
		{"home", UIKeys.Home.Keys(), "home"},
		{"end", UIKeys.End.Keys(), "end"},
		{"confirm", UIKeys.Confirm.Keys(), "enter"},
		{"toggle", UIKeys.Toggle.Keys(), "space"},
		{"cancel", UIKeys.Cancel.Keys(), "esc"},
		{"next field", UIKeys.NextField.Keys(), "tab"},
		{"prev field", UIKeys.PrevField.Keys(), "shift+tab"},
		{"vim up", UIKeys.VimUp.Keys(), "k"},
		{"vim down", UIKeys.VimDown.Keys(), "j"},
		{"vim left", UIKeys.VimLeft.Keys(), "h"},
		{"vim right", UIKeys.VimRight.Keys(), "l"},
		{"vim top", UIKeys.VimTop.Keys(), "g"},
		{"vim bottom", UIKeys.VimBottom.Keys(), "G"},
		{"list up", UIKeys.ListUp.Keys(), "ctrl+k"},
		{"list down", UIKeys.ListDown.Keys(), "ctrl+j"},
		{"quit", UIKeys.Quit.Keys(), "q"},
	} {
		assert.Equal(t, []string{tc.want}, tc.binding, tc.name)
	}
}
