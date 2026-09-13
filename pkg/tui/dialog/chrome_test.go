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
