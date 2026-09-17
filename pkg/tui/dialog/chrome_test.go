package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/help"
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
	require.True(t, d.ActionsFocused())
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
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

func TestDecisionDialogsExposeSafeCloseChrome(t *testing.T) {
	for _, d := range []Dialog{NewMaxIterationsDialog(10, "s", "request")} {
		policy, ok := d.(ClosePolicy)
		require.True(t, ok)
		assert.True(t, policy.DialogClosable())
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		require.NotNil(t, cmd)
		response, ok := firstMsgOfType[messages.InteractionResponseMsg](collectMsgs(cmd))
		require.True(t, ok)
		require.Equal(t, "s", response.SessionID)
		require.Equal(t, "request", response.Response.InteractionID)
		require.Equal(t, runtime.ResumeReject(""), response.Response.Resume)
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
	d.tab = tabBehavior
	d.selected[d.tab] = rowTabTitleLength
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

func TestSharedHeaderBodyGapIsSingleAndAdaptive(t *testing.T) {
	for _, header := range []string{"Title", "Title\n", "Title\n\n"} {
		headers, _ := bodyChrome(header, "Apply", 10)
		require.Equal(t, []string{"Title", ""}, headers)
		compact, _ := bodyChrome(header, "Apply", 3)
		require.Equal(t, []string{"Title"}, compact, "gap yields to an accessible body row")
	}
	b := BaseDialog{}
	b.SetSize(40, 12)
	footer := b.RenderActionKeys(34, "enter", "Apply")
	b.PrepareScrollableBody(styles.DialogStyle, 40, "Title\n\n", "Body", footer)
	before := b.TakeVisualDirty()
	require.True(t, before)
	view := b.RenderScrollableBody(styles.DialogStyle, 40, "Title\n\n", "Body", footer)
	row, col := b.CenterDialog(view)
	require.Equal(t, row+styles.DialogStyle.GetBorderTopSize()+styles.DialogStyle.GetPaddingTop()+2, b.bodyY)
	require.Equal(t, col+3, b.bodyX)
	require.False(t, b.TakeVisualDirty(), "rendering the prepared gap never changes viewport generation")
}

func TestDisabledActionHasNoClickableAuthorizationCells(t *testing.T) {
	b := BaseDialog{}
	b.SetSize(40, 12)
	footer := b.RenderActions(34, Action{Label: "Delete", Key: tea.KeyPressMsg{Code: 'd', Text: "d"}, Disabled: true}, Action{Label: "Cancel", Key: tea.KeyPressMsg{Code: tea.KeyEscape}})
	b.PrepareScrollableBody(styles.DialogStyle, 40, "Title", "Body", footer)
	view := b.RenderScrollableBody(styles.DialogStyle, 40, "Title", "Body", footer)
	row, col := b.CenterDialog(view)
	dl := NewDialogLayout(view, row, col)
	require.Contains(t, ansi.Strip(view), "Delete")
	cancelFound := false
	for y := row; y < row+dl.Height; y++ {
		for x := col; x < col+dl.Width; x++ {
			k, hit := b.ActionKeyAt(x, y, dl)
			if hit {
				require.NotEqual(t, 'd', k.Code)
				cancelFound = cancelFound || k.Code == tea.KeyEscape
			}
		}
	}
	require.True(t, cancelFound)
}

func TestSnapshotsCoalescedWheelScrollsPreparedBody(t *testing.T) {
	d := NewSnapshotsDialog(make([]int, 30)).(*snapshotsDialog)
	d.SetSize(60, 12)
	d.View()
	x, y, _, _ := d.BodyScrollBounds()
	before := d.BodyScrollOffset()
	_, cmd := d.Update(messages.WheelCoalescedMsg{Delta: 1, X: x, Y: y})
	require.Nil(t, cmd)
	require.Greater(t, d.BodyScrollOffset(), before)
}

func TestActionSectionNavigationSkipsDisabledAndRoutesOriginalKey(t *testing.T) {
	b := BaseDialog{}
	b.SetSize(50, 12)
	actions := []Action{
		{Label: "Unavailable", Key: tea.KeyPressMsg{Code: 'u'}, Disabled: true},
		{Label: "Apply", Key: tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}},
		{Label: "Skip", Key: tea.KeyPressMsg{Code: 'x'}, Disabled: true},
		{Label: "Open", Key: tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl}},
	}
	footer := b.RenderActions(44, actions...)
	b.PrepareScrollableBody(styles.DialogStyle, 50, "Title", "Input", footer)
	key, handled := b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyTab})
	require.True(t, handled)
	require.Zero(t, key.Code)
	require.True(t, b.ActionsFocused())
	require.Equal(t, 1, b.focusedAction)
	key, handled = b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyRight})
	require.True(t, handled)
	require.Zero(t, key.Code)
	require.Equal(t, 3, b.focusedAction)
	key, handled = b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, handled)
	require.Equal(t, "ctrl+enter", key.Keystroke())
	require.False(t, b.ActionsFocused())
	require.True(t, b.FocusActions(true))
	require.Equal(t, 3, b.focusedAction)
	key, handled = b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	require.True(t, handled)
	require.Zero(t, key.Code)
	require.False(t, b.ActionsFocused())
}

func TestActionDefaultMetadataRendersOneTruthfulEnterHint(t *testing.T) {
	b := BaseDialog{}
	actions := []Action{
		{Label: "No", Key: tea.KeyPressMsg{Code: 'n', Text: "n"}, Default: true, HideShortcut: true},
		{Label: "Execute", Key: tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl}},
	}
	view := b.RenderActions(100, actions...)
	plain := ansi.Strip(view)
	require.Contains(t, plain, "No ↵")
	require.NotContains(t, plain, "No ↵ n")
	require.Contains(t, plain, "Execute ctrl+enter")
	require.Equal(t, 1, strings.Count(plain, "↵"))
	require.NotContains(t, view, ";4m")
	require.NotContains(t, view, "[4m")
	b.FocusActions(true)
	plain = ansi.Strip(b.RenderActions(100, actions...))
	require.Contains(t, plain, "Execute ↵ ctrl+enter")
	require.NotContains(t, plain, "No ↵")
	require.Equal(t, 1, strings.Count(plain, "↵"))
	k, handled := b.HandleActionKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, handled)
	require.Equal(t, "ctrl+enter", k.Keystroke())
}

func TestTinyCloseControlHasOnlyVisibleExactCell(t *testing.T) {
	for _, size := range [][2]int{{1, 1}, {2, 3}, {3, 3}, {8, 4}, {16, 8}} {
		b := BaseDialog{}
		b.SetSize(size[0], size[1])
		view := b.RenderCard(styles.DialogStyle, size[0], "Title\nBody")
		dl := NewDialogLayout(view, 0, 0)
		for y := -1; y <= dl.Height; y++ {
			for x := -1; x <= dl.Width; x++ {
				hit := b.CloseButtonHit(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: y}, dl)
				if hit {
					require.GreaterOrEqual(t, x, 0)
					require.Less(t, x, dl.Width)
					require.GreaterOrEqual(t, y, 0)
					require.Less(t, y, dl.Height)
				}
			}
		}
		if dl.Width >= 3 && dl.Height >= 2 {
			require.Contains(t, ansi.Strip(view), dialogCloseGlyph)
		}
	}
}

func TestNestedOpenClearsParentPointerStateWithoutRestoringItOnPop(t *testing.T) {
	r := newDialogRuntime()
	mgr := New(r).(*manager)
	mgr.SetSize(80, 24)
	parent := NewExitConfirmationDialog().(*exitConfirmationDialog)
	mgr.handleOpen(OpenDialogMsg{Model: parent})
	settleTestDialog(mgr)
	parent.FocusActions(false)
	view := parent.View()
	row, col := parent.Position()
	x, y, visible := closeControlCell(lipgloss.Width(view), lipgloss.Height(view))
	require.True(t, visible)
	mgr.Update(tea.MouseMotionMsg{X: col + x, Y: row + y})
	require.True(t, parent.closeHovered)
	require.True(t, mgr.stack[0].closeHovered)
	mgr.View()
	mgr.TakeVisualDirty()
	mgr.drag.active = true
	child := NewHelpDialog(help.Document{})
	mgr.Update(OpenDialogMsg{Model: child})
	require.False(t, parent.closeHovered, "parent pointer state clears in the push Update, not on another motion")
	require.False(t, mgr.stack[0].closeHovered)
	require.False(t, mgr.drag.active)
	require.True(t, parent.ActionsFocused(), "keyboard section focus is not pointer hover")
	require.True(t, mgr.TakeVisualDirty())
	settleTestDialog(mgr)
	mgr.handleClose()
	require.Len(t, mgr.stack, 1)
	require.Same(t, parent, mgr.TopDialog())
	require.False(t, parent.closeHovered)
	require.False(t, mgr.stack[0].closeHovered)
	mgr.View()
	require.False(t, parent.closeHovered, "stationary pointer does not revive the occluded hover")
	mgr.Cleanup()
	require.Zero(t, r.ActiveCount())
}

func TestBodyActionGapIsDeliberateAndReclaimedWhenEmptyOrTiny(t *testing.T) {
	for _, height := range []int{4, 12} {
		b := BaseDialog{}
		b.SetSize(40, height)
		footer := b.RenderConfirmButtons(34)
		b.PrepareScrollableBody(styles.DialogStyle, 40, "Title", "Question", footer)
		view := b.RenderScrollableBody(styles.DialogStyle, 40, "Title", "Question", footer)
		row, col := b.CenterDialog(view)
		dl := NewDialogLayout(view, row, col)
		require.LessOrEqual(t, dl.Height, height)
		if height == 12 {
			require.Equal(t, 1, b.bodyFooterGap)
			require.Equal(t, b.bodyY+b.bodyHeight+1, row+b.actionFooterStart)
		} else {
			require.Zero(t, b.bodyFooterGap)
		}
		footer = b.RenderActions(34)
		b.PrepareScrollableBody(styles.DialogStyle, 40, "Title", "Question", footer)
		require.Zero(t, b.bodyFooterGap)
	}
}

func TestSharedTitleHeaderSpacingNormalizesCallerMargins(t *testing.T) {
	for _, header := range []string{"Title\nTabs\nFilter", "Title\n\nTabs\nFilter\n\n", "Title\n  \nTabs\nFilter\n\x1b[31m  \x1b[0m"} {
		headers, footers := bodyChrome(header, "Apply\n\n", 12)
		require.Equal(t, []string{"Title", "", "Tabs", "Filter", ""}, headers)
		require.Equal(t, []string{"Apply"}, footers)
		compact, _ := bodyChrome(header, "Apply", 5)
		require.Equal(t, []string{"Title", "Tabs", "Filter"}, compact, "spacing yields before header controls")
		tiny, _ := bodyChrome(header, "Apply", 3)
		require.Equal(t, []string{"Title"}, tiny)
	}
}

func TestSharedHeaderRowsMatchPaintAndDoNotTargetHiddenControls(t *testing.T) {
	for _, height := range []int{20, 7, 5, 3} {
		b := BaseDialog{}
		b.SetSize(40, height)
		footer := b.RenderActionKeys(34, "enter", "Apply")
		b.PrepareScrollableBody(styles.DialogStyle, 40, "Title\nTabs\nFilter", "Body", footer)
		view := b.RenderScrollableBody(styles.DialogStyle, 40, "Title\nTabs\nFilter", "Body", footer)
		row, _ := b.CenterDialog(view)
		lines := strings.Split(ansi.Strip(view), "\n")
		for index, label := range []string{"Title", "Tabs", "Filter"} {
			y, visible := b.headerRow(index)
			if visible {
				require.GreaterOrEqual(t, y-row, 0)
				require.Less(t, y-row, len(lines))
				require.Contains(t, lines[y-row], label)
			} else {
				require.NotContains(t, ansi.Strip(view), label)
			}
		}
		require.LessOrEqual(t, lipgloss.Height(view), height)
	}
}

func TestSharedChromePreservesMeaningfulBodyBlankRows(t *testing.T) {
	b := BaseDialog{}
	b.SetSize(40, 20)
	body := "\nFirst\n\nLast\n"
	footer := b.RenderActionKeys(34, "enter", "Apply")
	b.PrepareScrollableBody(styles.DialogStyle, 40, "Title\n\n", body, footer)
	view := b.RenderScrollableBody(styles.DialogStyle, 40, "Title\n\n", body, footer)
	row, col := b.CenterDialog(view)
	require.Equal(t, 5, b.bodyHeight)
	require.Equal(t, 1, b.bodyHeaderGap)
	require.Equal(t, 1, b.bodyFooterGap)
	lines := strings.Split(ansi.Strip(view), "\n")
	for i, want := range []string{"", "First", "", "Last", ""} {
		line := lines[b.bodyY-row+i]
		cells := []rune(line)
		got := strings.TrimSpace(string(cells[b.bodyX-col : b.bodyX-col+10]))
		require.Equal(t, want, got)
	}
}

func TestSharedTitleRemainsOneBoundedHeaderRow(t *testing.T) {
	for _, width := range []int{1, 2, 8, 24} {
		title := RenderTitle("Select Working Directory 界", width, styles.DialogTitleStyle)
		require.LessOrEqual(t, lipgloss.Width(title), width)
		require.Equal(t, 1, lipgloss.Height(title))
	}
}
