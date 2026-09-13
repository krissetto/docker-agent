package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func testAnimatedDialog(mgr *manager, d Dialog) *animatedDialog {
	animated, _ := newAnimatedDialog(mgr.runtime, d, mgr.width, mgr.height)
	animated.anim.Cancel()
	animated.disabled = true
	animated.renderAlpha = 1
	animated.renderWidth = animated.targetWidth
	animated.renderHeight = animated.targetHeight
	return animated
}

func settleTestDialog(mgr *manager) {
	entry := &mgr.stack[len(mgr.stack)-1]
	entry.anim.Cancel()
	entry.disabled = true
	entry.renderAlpha = 1
	entry.renderWidth = entry.targetWidth
	entry.renderHeight = entry.targetHeight
}

func TestExitConfirmationChromeSnapshot(t *testing.T) {
	d := NewExitConfirmationDialog().(*exitConfirmationDialog)
	d.SetSize(80, 24)
	view := ansi.Strip(d.View())
	assert.Contains(t, view, dialogCloseGlyph)
	assert.Contains(t, view, "No ↵")
	assert.Contains(t, view, "Yes")
	assert.NotContains(t, view, "Y yes")
}

func TestExitConfirmationKeyboardParity(t *testing.T) {
	d := NewExitConfirmationDialog().(*exitConfirmationDialog)
	d.SetSize(80, 24)

	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, cmd)
	_, ok := cmd().(CloseDialogMsg)
	assert.True(t, ok, "Enter activates the default No pill")

	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.Contains(t, ansi.Strip(d.View()), "Yes ↵")
	_, cmd = d.Update(tea.KeyPressMsg{Text: "n"})
	require.NotNil(t, cmd)
	_, ok = cmd().(CloseDialogMsg)
	assert.True(t, ok, "N remains a direct keyboard shortcut")
}

func TestExitConfirmationPillAndCloseHitboxes(t *testing.T) {
	d := NewExitConfirmationDialog().(*exitConfirmationDialog)
	d.SetSize(80, 24)
	dl := d.layout()

	closeClick := tea.MouseClickMsg{Button: tea.MouseLeft, X: dl.Col + dl.Width - 3, Y: dl.Row + 1}
	_, cmd := d.Update(closeClick)
	require.NotNil(t, cmd)
	_, ok := cmd().(CloseDialogMsg)
	assert.True(t, ok)

	lines := strings.Split(ansi.Strip(dl.View), "\n")
	buttonY := -1
	for i, line := range lines {
		if strings.Contains(line, "No ↵") && strings.Contains(line, "Yes") {
			buttonY = dl.Row + i
		}
	}
	require.NotEqual(t, -1, buttonY)
	style := stylesForTest()
	contentLeft := dl.Col + style.GetBorderLeftSize() + style.GetPaddingLeft()
	_, cmd = d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: contentLeft + d.confirmBtnNoX, Y: buttonY})
	require.NotNil(t, cmd)
	_, ok = cmd().(CloseDialogMsg)
	assert.True(t, ok, "entire No pill is clickable from its first cell")
}

func TestManagerCloseHoverIsScopedToDialogLifecycle(t *testing.T) {
	mgr := New(animation.NewRuntime()).(*manager)
	mgr.SetSize(80, 24)

	a := NewExitConfirmationDialog()
	_, _ = mgr.Update(OpenDialogMsg{Model: a})
	settleTestDialog(mgr)
	row, col := a.Position()
	closeX := col + lipgloss.Width(a.View()) - styles.DialogStyle.GetBorderRightSize() - 1 - dialogCloseInset
	closeY := row + styles.DialogStyle.GetBorderTopSize()
	_, _ = mgr.Update(tea.MouseMotionMsg{X: closeX, Y: closeY})
	require.True(t, mgr.stack[len(mgr.stack)-1].closeHovered)
	require.True(t, a.(*exitConfirmationDialog).closeHovered)

	_, closeCmd := mgr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: closeX, Y: closeY})
	require.NotNil(t, closeCmd)
	_, _ = mgr.Update(closeCmd())

	b := NewExitConfirmationDialog()
	// Even a reused model carrying stale component-local state starts a new
	// open lifecycle unhovered, without requiring synthetic pointer motion.
	b.(*exitConfirmationDialog).closeHovered = true
	_, _ = mgr.Update(OpenDialogMsg{Model: b})
	settleTestDialog(mgr)
	top := &mgr.stack[len(mgr.stack)-1]
	assert.False(t, top.closeHovered)
	assert.False(t, b.(*exitConfirmationDialog).closeHovered)

	row, col = b.Position()
	unhoveredB := b.View()
	closeX = col + lipgloss.Width(unhoveredB) - styles.DialogStyle.GetBorderRightSize() - 1 - dialogCloseInset
	closeY = row + styles.DialogStyle.GetBorderTopSize()
	_, _ = mgr.Update(tea.MouseMotionMsg{X: closeX, Y: closeY})
	assert.NotEqual(t, unhoveredB, b.View(), "fresh B motion activates close hover visually")
	assert.True(t, mgr.stack[len(mgr.stack)-1].closeHovered, "fresh B motion activates manager hover")
	assert.True(t, b.(*exitConfirmationDialog).closeHovered, "fresh B motion activates component hover")
}

func TestExitConfirmationDismissalNeverConfirms(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{name: "escape", msg: tea.KeyPressMsg{Code: tea.KeyEscape}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := New(animation.NewRuntime()).(*manager)
			mgr.SetSize(80, 24)
			d := NewExitConfirmationDialog()
			d.SetSize(80, 24)
			mgr.stack = []dialogEntry{{animatedDialog: testAnimatedDialog(mgr, d)}}

			_, cmd := mgr.Update(tc.msg)
			require.NotNil(t, cmd)
			msgs := collectMsgs(cmd)
			assert.Equal(t, []tea.Msg{CloseDialogMsg{}}, msgs)
			assert.False(t, hasDialogMsg[ExitConfirmedMsg](msgs))
			assert.False(t, hasDialogMsg[tea.QuitMsg](msgs))
		})
	}
}

func hasDialogMsg[T tea.Msg](msgs []tea.Msg) bool {
	for _, msg := range msgs {
		if _, ok := msg.(T); ok {
			return true
		}
	}
	return false
}

type chromeTestDialog struct{ BaseDialog }

func (d *chromeTestDialog) Init() tea.Cmd                          { return nil }
func (d *chromeTestDialog) Update(tea.Msg) (layout.Model, tea.Cmd) { return d, nil }
func (d *chromeTestDialog) View() string                           { return "box" }
func (d *chromeTestDialog) Position() (int, int)                   { return 10, 10 }

func stylesForTest() lipgloss.Style { return styles.DialogStyle.Padding(1, 2) }

type managerChromeTestDialog struct{ chromeTestDialog }

func (d *managerChromeTestDialog) View() string { return styles.DialogStyle.Width(20).Render("box") }

func TestManagerAddsCloseChromeToPlainDialog(t *testing.T) {
	mgr := New(animation.NewRuntime()).(*manager)
	mgr.SetSize(40, 12)
	d := &managerChromeTestDialog{}
	d.SetSize(40, 12)
	mgr.stack = []dialogEntry{{animatedDialog: testAnimatedDialog(mgr, d)}}
	assert.Contains(t, ansi.Strip(mgr.View()), dialogCloseGlyph)
}

func TestMandatoryDialogsExplicitlyDisableCloseChrome(t *testing.T) {
	for _, d := range []Dialog{NewMaxIterationsDialog(10, "s", "request")} {
		policy, ok := d.(ClosePolicy)
		require.True(t, ok)
		assert.False(t, policy.DialogClosable())
	}
}

func TestPickerSizeNeverExceedsRoot(t *testing.T) {
	p := newPickerCore(commandPaletteLayout, "search")
	p.SetSize(24, 6)
	w, h, c := p.dialogSize()
	assert.LessOrEqual(t, w, 24)
	assert.LessOrEqual(t, h, 6)
	assert.GreaterOrEqual(t, c, 1)
	p.SetSize(120, 40)
	w, h, c = p.dialogSize()
	assert.LessOrEqual(t, w, 120)
	assert.LessOrEqual(t, h, 40)
	assert.Greater(t, c, 1)
}

func TestWrappedConfirmPillsRemainCellAddressable(t *testing.T) {
	for _, size := range [][2]int{{16, 8}, {20, 6}, {40, 12}} {
		b := BaseDialog{}
		b.SetSize(size[0], size[1])
		width := b.ComputeDialogWidth(100, 16, 40)
		footer := b.RenderConfirmButtons(b.ContentWidth(width, 2))
		b.PrepareScrollableBody(styles.DialogStyle, width, "Confirm", "Long confirmation body that must remain scrollable.", footer)
		view := b.RenderScrollableBody(styles.DialogStyle, width, "Confirm", "Long confirmation body that must remain scrollable.", footer)
		row, col := b.CenterDialog(view)
		dl := NewDialogLayout(view, row, col)
		for _, want := range []rune{'n', 'y'} {
			found := false
			for y := row; y < row+dl.Height; y++ {
				for x := col; x < col+dl.Width; x++ {
					if k, ok := b.ActionKeyAt(x, y, dl); ok && k.Code == want {
						found = true
					}
				}
			}
			require.True(t, found, "%dx%d: action %c must have visible clickable cells\n%s", size[0], size[1], want, ansi.Strip(view))
		}
	}
}

func TestTinyFooterScrollKeepsEveryActionReachable(t *testing.T) {
	b := BaseDialog{}
	b.SetSize(20, 6)
	actions := []Action{{Label: "First action", Key: tea.KeyPressMsg{Code: 'a'}}, {Label: "Second action", Key: tea.KeyPressMsg{Code: 'b'}}, {Label: "Third action", Key: tea.KeyPressMsg{Code: 'c'}}, {Label: "Fourth action", Key: tea.KeyPressMsg{Code: 'd'}}, {Label: "Fifth action", Key: tea.KeyPressMsg{Code: 'e'}}}
	footer := b.RenderActions(b.ContentWidth(20, 2), actions...)
	seen := map[rune]bool{}
	for range 5 {
		b.PrepareScrollableBody(styles.DialogStyle, 20, "Title", "body", footer)
		view := b.RenderScrollableBody(styles.DialogStyle, 20, "Title", "body", footer)
		require.True(t, b.actionScrollActive)
		row, col := b.CenterDialog(view)
		dl := NewDialogLayout(view, row, col)
		for y := row; y < row+dl.Height; y++ {
			for x := col; x < col+dl.Width; x++ {
				if k, ok := b.ActionKeyAt(x, y, dl); ok {
					seen[k.Code] = true
				}
			}
		}
		b.actionScroll.ScrollBy(1)
	}
	for _, action := range actions {
		require.True(t, seen[action.Key.Code], "action %s must remain scroll-reachable", action.Label)
	}
}

func TestBodyViewportDoesNotStealFormNavigationOrWrapPadding(t *testing.T) {
	b := BaseDialog{}
	b.SetSize(40, 12)
	body := "First field" + strings.Repeat(" ", 30) + "\nSecond field"
	b.PrepareScrollableBody(styles.DialogStyle, 40, "Title", body, b.RenderActionKeys(34, "enter", "Save"))
	b.RenderScrollableBody(styles.DialogStyle, 40, "Title", body, b.RenderActionKeys(34, "enter", "Save"))
	require.Equal(t, 2, b.bodyScroll.MaxScrollOffset()+b.bodyScroll.VisibleHeight())
	for _, k := range []tea.KeyPressMsg{{Code: 'j', Text: "j"}, {Code: 'k', Text: "k"}, {Code: tea.KeyUp}, {Code: tea.KeyDown}, {Code: tea.KeyHome}, {Code: tea.KeyEnd}} {
		handled, _ := b.UpdateBodyScroll(k)
		require.False(t, handled, "form owns %s", k.String())
	}
}

func TestBodyTextCannotActivateScrolledOutAction(t *testing.T) {
	b := BaseDialog{}
	b.SetSize(20, 6)
	footer := b.RenderActions(14,
		Action{Label: "Allow once", Key: tea.KeyPressMsg{Code: 'a'}},
		Action{Label: "Always allow", Key: tea.KeyPressMsg{Code: 'b'}},
		Action{Label: "Reject action", Key: tea.KeyPressMsg{Code: 'c'}},
		Action{Label: "Cancel", Key: tea.KeyPressMsg{Code: 'd'}})
	b.PrepareScrollableBody(styles.DialogStyle, 20, "Title", "Allow once", footer)
	b.RenderScrollableBody(styles.DialogStyle, 20, "Title", "Allow once", footer)
	require.True(t, b.actionScrollActive)
	b.actionScroll.ScrollToBottom()
	b.PrepareScrollableBody(styles.DialogStyle, 20, "Title", "Allow once", footer)
	view := b.RenderScrollableBody(styles.DialogStyle, 20, "Title", "Allow once", footer)
	row, col := b.CenterDialog(view)
	dl := NewDialogLayout(view, row, col)
	x, y, w, h := b.BodyScrollBounds()
	require.Positive(t, h)
	for cell := x; cell < x+w; cell++ {
		_, hit := b.ActionKeyAt(cell, y, dl)
		require.False(t, hit, "body text is never an action even when the pill is offscreen")
	}
}

func TestPartialActionCannotAuthorizeBorderOrOutsideCells(t *testing.T) {
	for _, inner := range []int{1, 2} {
		b := BaseDialog{}
		b.SetSize(inner+2, 8)
		style := styles.DialogStyle.Padding(0)
		footer := b.RenderActions(inner, Action{Label: "Allow", Key: tea.KeyPressMsg{Code: 'a'}})
		b.PrepareScrollableBody(style, inner+2, "", "body", footer)
		view := b.RenderScrollableBody(style, inner+2, "", "body", footer)
		row, col := b.CenterDialog(view)
		dl := NewDialogLayout(view, row, col)
		for y := row; y < row+dl.Height; y++ {
			for _, x := range []int{col - 1, col, col + dl.Width - 1, col + dl.Width} {
				_, hit := b.ActionKeyAt(x, y, dl)
				require.False(t, hit, "borders/outside never authorize: inner=%d x=%d y=%d", inner, x, y)
			}
		}
	}
}

func TestDraggedCoalescedWheelRoutesToVisibleFooter(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(30, 10)
	d := NewSettingsDialog(messages.Preferences{}, true).(*settingsDialog)
	mgr.handleOpen(OpenDialogMsg{Model: d})
	settleTestDialog(mgr)
	// Use a tiny card in a taller root so its footer overflows and the entry can move.
	d.SetSize(20, 6)
	mgr.stack[0].invalidateView()
	view := d.View()
	mgr.stack[0].renderWidth, mgr.stack[0].renderHeight = lipgloss.Width(view), lipgloss.Height(view)
	mgr.stack[0].offsetY = 2
	require.True(t, d.actionScrollActive)
	_, bodyY, _, bodyHeight := d.BodyScrollBounds()
	before := d.actionScroll.ScrollOffset()
	_, cmd := mgr.Update(messages.WheelCoalescedMsg{Delta: 1, X: 4, Y: bodyY + bodyHeight + 2})
	require.Nil(t, cmd)
	require.Greater(t, d.actionScroll.ScrollOffset(), before)
	bodyBefore := d.BodyScrollOffset()
	_, cmd = mgr.Update(messages.WheelCoalescedMsg{Delta: 1, X: 4, Y: bodyY + 2})
	require.Nil(t, cmd)
	require.Greater(t, d.BodyScrollOffset(), bodyBefore)
	mgr.Cleanup()
}

func TestActionModifierKeysPreserveSemanticKeystroke(t *testing.T) {
	b := BaseDialog{}
	b.SetSize(60, 12)
	footer := b.RenderActionKeys(40, "ctrl+s", "Apply", "alt+h", "Hidden")
	b.PrepareScrollableBody(styles.DialogStyle, 46, "Title", "body", footer)
	view := b.RenderScrollableBody(styles.DialogStyle, 46, "Title", "body", footer)
	row, col := b.CenterDialog(view)
	dl := NewDialogLayout(view, row, col)
	seen := map[string]bool{}
	for y := row; y < row+dl.Height; y++ {
		for x := col; x < col+dl.Width; x++ {
			if k, hit := b.ActionKeyAt(x, y, dl); hit {
				seen[k.String()] = true
			}
		}
	}
	require.True(t, seen["ctrl+s"])
	require.True(t, seen["alt+h"])
	require.False(t, seen["s"])
	require.False(t, seen["h"])
}
