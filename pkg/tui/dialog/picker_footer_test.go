package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestPickerFooterOneButtonPlainHintsAndExactHits(t *testing.T) {
	for _, width := range []int{12, 24, 60, 120} {
		b := BaseDialog{}
		b.SetSize(width+6, 30)
		actions := actionsForKeys("ctrl+s", "Star", "ctrl+f", "Filter stars", "ctrl+y", "Copy ID", "ctrl+d", "Delete", "enter", "Load")
		footer := b.RenderPickerFooter(width, actions...)
		b.PrepareScrollableBody(styles.DialogStyle, width+6, "Sessions", "body", footer)
		view := b.RenderScrollableBody(styles.DialogStyle, width+6, "Sessions", "body", footer)
		row, col := b.CenterDialog(view)
		dl := NewDialogLayout(view, row, col)
		cells := uv.NewStyledString(footer).Lines(ansi.GraphemeWidth)
		seen := map[string]bool{}
		for y, entry := range b.actionRows {
			for _, hit := range entry.hits {
				seen[hit.key.Keystroke()] = true
				for x := hit.x; x < hit.x+hit.width; x++ {
					if hit.key.Code == tea.KeyEnter {
						assert.NotNil(t, cells[y][x].Style.Bg)
					} else {
						assert.Nil(t, cells[y][x].Style.Bg, "hints never have button backgrounds")
					}
					key, ok := b.ActionKeyAt(col+b.actionContentX+x, row+b.actionFooterStart+y, dl)
					require.True(t, ok)
					assert.Equal(t, hit.key, key)
				}
			}
			for x := range lipgloss.Width(entry.text) {
				belongs := false
				for _, hit := range entry.hits {
					belongs = belongs || (x >= hit.x && x < hit.x+hit.width)
				}
				if !belongs {
					_, hit := b.ActionKeyAt(col+b.actionContentX+x, row+b.actionFooterStart+y, dl)
					assert.False(t, hit, "gaps cannot activate")
				}
			}
		}
		assert.Len(t, seen, 5)
		assert.Contains(t, ansi.Strip(footer), "ctrl+s Star")
		t.Logf("Sessions footer width%d:\n%s", width, ansi.Strip(footer))
	}
}

func TestPickerFooterHoverAndFocusRemainUnboxed(t *testing.T) {
	r := newDialogRuntime()
	b := BaseDialog{}
	b.SetSize(70, 20)
	actions := actionsForKeys("ctrl+s", "Star 界", "enter", "Load")
	render := func() string {
		footer := b.RenderPickerFooter(64, actions...)
		b.PrepareScrollableBody(styles.DialogStyle, 70, "Title", "Body", footer)
		return b.RenderScrollableBody(styles.DialogStyle, 70, "Title", "Body", footer)
	}
	view := render()
	row, col := b.CenterDialog(view)
	x, y := col+b.actionContentX, row+b.actionFooterStart
	b.UpdateFooterHover(tea.MouseMotionMsg{X: x, Y: y}, r, NewDialogLayout(view, row, col))
	require.True(t, b.footerHover.Running())
	for b.footerHover.Running() {
		tick := acceptedDialogTick(r, r.Continue())
		b.UpdateFooterHover(tick, r, NewDialogLayout(view, row, col))
	}
	b.FocusActions(false)
	footer := b.RenderPickerFooter(64, actions...)
	cells := uv.NewStyledString(strings.Split(footer, "\n")[0]).Lines(ansi.GraphemeWidth)[0]
	for _, hit := range b.actionRows[0].hits {
		if hit.key.Code != tea.KeyEnter {
			for x := hit.x; x < hit.x+hit.width; x++ {
				assert.Nil(t, cells[x].Style.Bg)
			}
		}
	}
	b.StopFooterHover()
	require.Zero(t, r.ActiveCount())
}

func TestPickerFooterTinyViewportRevealsDisabledSafeActions(t *testing.T) {
	b := BaseDialog{}
	b.SetSize(24, 6)
	actions := actionsForKeys("ctrl+d", "Delete", "ctrl+y", "Copy ID", "enter", "Load")
	actions[0].Disabled = true
	footer := b.RenderPickerFooter(18, actions...)
	b.PrepareScrollableBody(styles.DialogStyle, 24, "Title", "body", footer)
	require.True(t, b.FocusActions(false))
	key, _ := b.SelectedActionKey()
	assert.Equal(t, "ctrl+y", key.Keystroke())
	b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyRight})
	key, handled := b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, handled)
	assert.Equal(t, tea.KeyEnter, key.Code)
}
