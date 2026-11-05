package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func themeChoicesWithDockerNext() []ThemeChoice {
	return []ThemeChoice{
		{Ref: "docker-next-dark", Name: "Docker Next - Dark", IsBuiltin: true, IsDefault: true, IsCurrent: true},
		{Ref: "docker-next-light", Name: "Docker Next - Light", IsBuiltin: true},
		{Ref: "default", Name: "Old School", IsBuiltin: true},
		{Ref: "dracula", Name: "Dracula", IsBuiltin: true},
		{Ref: "black-amber", Name: "Black Amber", IsBuiltin: true},
	}
}

// themeRowY returns the screen row of the list line that renders the item at idx,
// computed from the body origin the mouse hit-test reads (bodyY).
func themeRowY(t *testing.T, d *themePickerDialog, idx int) int {
	t.Helper()
	for line := range 1000 {
		if d.lineToThemeIndex(line) != idx {
			continue
		}
		require.GreaterOrEqual(t, line-d.scrollview.ScrollOffset(), 0, "row scrolled above the list")
		require.Less(t, line-d.scrollview.ScrollOffset(), d.scrollview.VisibleHeight(), "row is not visible")
		return d.bodyY + (line - d.scrollview.ScrollOffset())
	}
	require.FailNow(t, "no line renders the item")
	return 0
}

func indexOfTheme(t *testing.T, d *themePickerDialog, ref string) int {
	t.Helper()
	for i, c := range d.filtered {
		if c.Ref == ref {
			return i
		}
	}
	require.FailNow(t, "theme not listed", ref)
	return -1
}

// clickThemeRow sends a left click one column inside the list body.
// containsPreview reports whether msgs hold a preview of ref against originalRef.
// The preview also carries a revision counter, which the test does not pin.
func containsPreview(msgs []tea.Msg, ref, originalRef string) bool {
	for _, msg := range msgs {
		if p, ok := msg.(messages.ThemePreviewMsg); ok && p.ThemeRef == ref && p.OriginalRef == originalRef {
			return true
		}
	}
	return false
}

func clickThemeRow(d *themePickerDialog, y int) tea.Cmd {
	_, cmd := d.Update(tea.MouseClickMsg{X: d.bodyX + 1, Y: y, Button: tea.MouseLeft})
	return cmd
}

// A real mouse click previews a Docker Next palette, a second click on the same
// row applies it, and clicking the default row applies the default again. The
// selection, hit-test and emitted messages all go through the picker's real
// mouse path, not keyboard navigation.
func TestThemePickerMouseSelectsDockerNextPaletteAndRestoresDefault(t *testing.T) {
	t.Parallel()

	d := NewThemePickerDialog(themeChoicesWithDockerNext(), "docker-next-dark").(*themePickerDialog)
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 60})

	for _, ref := range []string{"docker-next-light", "black-amber"} {
		idx := indexOfTheme(t, d, ref)
		y := themeRowY(t, d, idx)

		// Single click: the row becomes the selection and a preview is emitted.
		msgs := collectMsgs(clickThemeRow(d, y))
		assert.Equal(t, idx, d.selected, ref)
		assert.True(t, containsPreview(msgs, ref, "docker-next-dark"), ref)

		// Second click on the same row within the threshold applies it.
		msgs = collectMsgs(clickThemeRow(d, y))
		assert.Contains(t, msgs, messages.ChangeThemeMsg{ThemeRef: ref}, ref)
	}

	// Restore the default through the same mouse path.
	defIdx := indexOfTheme(t, d, "docker-next-dark")
	defY := themeRowY(t, d, defIdx)
	collectMsgs(clickThemeRow(d, defY))
	msgs := collectMsgs(clickThemeRow(d, defY))
	assert.Contains(t, msgs, messages.ChangeThemeMsg{ThemeRef: "docker-next-dark"})
}

// Clicks outside the list rows (above the list) do not select anything, so the
// hit-test bounds are the list rows only and do not depend on palette colour.
func TestThemePickerMouseOutsideListIsIgnored(t *testing.T) {
	t.Parallel()

	d := NewThemePickerDialog(themeChoicesWithDockerNext(), "docker-next-dark").(*themePickerDialog)
	d.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
	before := d.selected
	dialogRow, _ := d.Position()
	msgs := collectMsgs(clickThemeRow(d, dialogRow))
	assert.Equal(t, before, d.selected)
	assert.Empty(t, msgs)
}
