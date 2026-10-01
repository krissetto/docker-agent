package dialog

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func newSizedEditDialog(kind, value string) (Dialog, *editor.Input, *BaseDialog) {
	if kind == "todo" {
		d := NewTodoEditDialog(messages.TodoScope{}, "id", value, 1, nil).(*todoEditDialog)
		return d, d.input, &d.BaseDialog
	}
	d := NewPendingMessageEditDialog("session", "turn", value, 1, nil).(*pendingMessageEditDialog)
	return d, d.input, &d.BaseDialog
}

func TestEditDialogThreeTextRowsGrowAndShrink(t *testing.T) {
	for _, kind := range []string{"todo", "queued"} {
		t.Run(kind, func(t *testing.T) {
			d, input, base := newSizedEditDialog(kind, "short draft")
			d.SetSize(100, 40)
			compactHeight := lipgloss.Height(d.View())
			require.Equal(t, 3, input.Height(), "baseline counts text rows, not frame padding")
			require.Less(t, compactHeight, 20)
			require.Equal(t, 85, lipgloss.Width(d.View()), "retain the existing width policy")
			t.Log("short editor:\n" + ansi.Strip(d.View()))

			d.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
			d.Update(tea.PasteMsg{Content: "\nsecond\nthird"})
			require.Equal(t, 3, input.Height())
			require.Equal(t, compactHeight, lipgloss.Height(d.View()))
			d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
			require.Equal(t, 4, input.Height())
			require.Equal(t, compactHeight+1, lipgloss.Height(d.View()))
			d.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
			require.Equal(t, 3, input.Height())
			require.Equal(t, compactHeight, lipgloss.Height(d.View()))

			d.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
			d.Update(tea.PasteMsg{Content: strings.Repeat("wide 界é👩‍💻 words ", 35)})
			require.Greater(t, input.ContentLineCount(), 3)
			require.Equal(t, input.ContentLineCount(), input.Height(), "use the editor's actual grapheme-aware wraps")
			require.Equal(t, compactHeight+input.Height()-3, lipgloss.Height(d.View()))
			require.Zero(t, input.ScrollYOffset(), "all wrapped rows fit without scrolling")
			require.Equal(t, input.SurfaceHeight(), base.bodyHeight)
			t.Log("wrapped editor:\n" + ansi.Strip(d.View()))
			wideRows := input.Height()
			d.Update(tea.WindowSizeMsg{Width: 70, Height: 40})
			require.Greater(t, input.Height(), wideRows)
			require.Equal(t, input.ContentLineCount(), input.Height())
			d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
			require.Equal(t, wideRows, input.Height())

			d.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
			d.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
			require.Empty(t, input.Value())
			require.Equal(t, 3, input.Height())
			require.Equal(t, compactHeight, lipgloss.Height(d.View()))
			require.Zero(t, input.ScrollYOffset())
		})
	}
}

func TestEditDialogCappedInputScrollAndResizedHitboxes(t *testing.T) {
	for _, kind := range []string{"todo", "queued"} {
		t.Run(kind, func(t *testing.T) {
			d, input, base := newSizedEditDialog(kind, "first")
			d.SetSize(80, 24)
			d.Update(tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl})
			content := "first" + strings.Repeat("\n界é👩‍💻 row", 100) + "\nlast"
			d.Update(tea.PasteMsg{Content: strings.TrimPrefix(content, "first")})
			require.Equal(t, content, input.Value())
			require.Equal(t, 24, lipgloss.Height(d.View()))
			require.Less(t, input.Height(), input.ContentLineCount())
			require.Positive(t, input.ScrollYOffset())
			require.Zero(t, base.BodyScrollOffset(), "editor scrolls rather than the surrounding dialog")
			require.Contains(t, ansi.Strip(d.View()), "last")
			before := input.ScrollYOffset()
			x, y, _, _ := base.BodyScrollBounds()
			d.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
			require.Equal(t, before, input.ScrollYOffset(), "cursor moves within the viewport before scrolling")
			for range input.Height() {
				d.Update(tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelUp})
			}
			require.Less(t, input.ScrollYOffset(), before)
			line, column, offset := input.Line(), input.Column(), input.ScrollYOffset()
			view := d.View()
			for range 4 {
				d.Update(nil)
				require.Equal(t, line, input.Line())
				require.Equal(t, column, input.Column())
				require.Equal(t, offset, input.ScrollYOffset(), "layout preparation preserves the scrolled viewport")
				require.Equal(t, view, d.View())
			}
			require.GreaterOrEqual(t, line, offset)
			require.Less(t, line, offset+input.Height(), "cursor remains visible in the capped editor")
			d.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
			require.Zero(t, input.ScrollYOffset())
			require.Contains(t, ansi.Strip(d.View()), "first")
			frame := input.Frame()
			d.Update(tea.MouseClickMsg{X: x + frame.GetHorizontalFrameSize()/2 + 2, Y: y + frame.GetPaddingTop() + 1, Button: tea.MouseLeft})
			require.Equal(t, 1, input.Line())
			require.Equal(t, 1, input.Column(), "click follows wide glyph geometry after growth")

			d.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
			d.Update(tea.PasteMsg{Content: "small"})
			require.Equal(t, 3, input.Height())
			cx, cy := familyActionCell(t, d, "Cancel")
			_, cmd := d.Update(tea.MouseClickMsg{X: cx, Y: cy, Button: tea.MouseLeft})
			require.NotNil(t, cmd, "action hitbox follows the shrunken footer")
			require.IsType(t, CloseDialogByModelMsg{}, cmd())
		})
	}
}

func TestEditDialogContentSizingTinyScreens(t *testing.T) {
	for _, kind := range []string{"todo", "queued"} {
		for _, size := range [][2]int{{40, 12}, {24, 8}, {10, 4}, {3, 2}, {1, 1}, {0, 0}} {
			t.Run(fmt.Sprintf("%s/%dx%d", kind, size[0], size[1]), func(t *testing.T) {
				d, input, _ := newSizedEditDialog(kind, strings.Repeat("界é👩‍💻\n", 50))
				d.SetSize(size[0], size[1])
				view := d.View()
				require.LessOrEqual(t, lipgloss.Height(view), max(1, size[1]))
				require.LessOrEqual(t, lipgloss.Width(view), max(1, size[0]))
				require.Positive(t, input.Height())
				require.Positive(t, input.Width())
				t.Log("tiny editor:\n" + ansi.Strip(view))
				d.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
				d.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
				d.Update(tea.PasteMsg{Content: "restored"})
				require.Equal(t, 3, input.Height())
				require.Contains(t, ansi.Strip(d.View()), "restored")
			})
		}
	}
}

func TestEditDialogContentResizeUsesAnimatedGeometry(t *testing.T) {
	for _, kind := range []string{"todo", "queued"} {
		t.Run(kind, func(t *testing.T) {
			r := newDialogRuntime()
			mgr := &manager{runtime: r, width: 100, height: 40}
			d, _, _ := newSizedEditDialog(kind, "draft")
			mgr.handleOpen(OpenDialogMsg{Model: d})
			require.True(t, mgr.stack[0].anim.Running(), "editor retains the shared opening reveal")
			advanceResize(mgr, dialogOpenDuration)
			t.Cleanup(mgr.Cleanup)
			compact := resizeRect(mgr)
			for _, growing := range []bool{true, false} {
				before := resizeRect(mgr)
				mgr.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
				value := "draft"
				if growing {
					value = strings.Repeat("more rows\n", 12)
				}
				mgr.Update(tea.PasteMsg{Content: value})
				require.Equal(t, before, resizeRect(mgr), "content changes do not snap the displayed rectangle")
				require.True(t, mgr.pointerSuppressed(), "transition cannot target stale hitboxes")
				advanceResize(mgr, dialogResizeDuration/2)
				middle := resizeRect(mgr)
				target := mgr.stack[0].targetHeight
				require.Greater(t, middle[3], min(before[3], target))
				require.Less(t, middle[3], max(before[3], target))
				require.Equal(t, 1.0, mgr.stack[0].opacity(), "resize is geometry, not a fade")
				advanceResize(mgr, dialogResizeDuration)
				require.Equal(t, d.View(), mgr.stack[0].view())
				require.False(t, mgr.pointerSuppressed())
			}
			require.Equal(t, compact, resizeRect(mgr))
			require.Zero(t, r.ActiveCount())
		})
	}
}
