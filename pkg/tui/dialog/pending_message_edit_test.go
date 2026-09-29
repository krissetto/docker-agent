package dialog

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestPendingMessageEditSaveKeepsFullContentAndIdentity(t *testing.T) {
	calls := 0
	content := strings.Repeat("queued 界e\u0301\n", 150) + "  trailing  "
	d := NewPendingMessageEditDialog("session", "turn", content, 7, func(value string) tea.Cmd {
		calls++
		assert.Equal(t, content, value)
		return func() tea.Msg { return PendingMessageSaveResultMsg{SessionID: "session", TurnID: "turn", EditorID: 7} }
	}).(*pendingMessageEditDialog)
	d.SetSize(70, 20)
	assert.Equal(t, content, d.input.Value(), "multiline drafts have no truncation cap")
	assert.Equal(t, uint64(7), d.PendingEditID())
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	require.NotNil(t, cmd)
	assert.True(t, d.saving)
	_, duplicate := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	assert.Nil(t, duplicate)
	assert.Equal(t, 1, calls)
	result := cmd()
	_, closeCmd := d.Update(result)
	require.NotNil(t, closeCmd)
	closeMsg, ok := closeCmd().(CloseDialogByModelMsg)
	require.True(t, ok)
	assert.Same(t, d, closeMsg.Model)
	_, closeCmd = d.Update(result)
	assert.Nil(t, closeCmd, "duplicate async success cannot close another modal")
}

func TestPendingMessageEditFailuresRetainDraft(t *testing.T) {
	for _, failure := range []string{"message already claimed", "persist failed", "editing unsupported"} {
		t.Run(failure, func(t *testing.T) {
			d := NewPendingMessageEditDialog("session", "turn", "draft\nsecond line", 8, func(string) tea.Cmd {
				return func() tea.Msg {
					return PendingMessageSaveResultMsg{SessionID: "session", TurnID: "turn", EditorID: 8, Err: errors.New(failure)}
				}
			}).(*pendingMessageEditDialog)
			d.SetSize(50, 14)
			_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
			_, closeCmd := d.Update(cmd())
			assert.Nil(t, closeCmd)
			assert.False(t, d.saving)
			assert.False(t, d.closed)
			assert.Equal(t, "draft\nsecond line", d.input.Value())
			assert.Contains(t, ansi.Strip(d.View()), failure)
			assert.True(t, d.input.Focused())
		})
	}
}

func TestPendingMessageEditCancelNeverSavesAndIgnoresLateResults(t *testing.T) {
	calls := 0
	d := NewPendingMessageEditDialog("session", "turn", "draft", 9, func(string) tea.Cmd { calls++; return nil }).(*pendingMessageEditDialog)
	d.SetSize(60, 20)
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd)
	assert.IsType(t, CloseDialogByModelMsg{}, cmd())
	assert.Zero(t, calls)
	_, cmd = d.Update(PendingMessageSaveResultMsg{SessionID: "session", TurnID: "turn", EditorID: 9, Err: errors.New("late")})
	assert.Nil(t, cmd)
	assert.Empty(t, d.saveError)
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	assert.Nil(t, cmd)
	assert.Zero(t, calls)
}

func TestPendingMessageEditResultCannotReachReopenedEditor(t *testing.T) {
	d := NewPendingMessageEditDialog("session", "turn", "new draft", 12, func(string) tea.Cmd { return func() tea.Msg { return nil } }).(*pendingMessageEditDialog)
	d.SetSize(60, 20)
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	_, cmd := d.Update(PendingMessageSaveResultMsg{SessionID: "session", TurnID: "turn", EditorID: 11})
	assert.Nil(t, cmd)
	assert.True(t, d.saving)
	assert.False(t, d.closed)
	assert.Equal(t, "new draft", d.input.Value())
}

func TestPendingMessageEditKeyboardAndMouseSave(t *testing.T) {
	calls := 0
	d := NewPendingMessageEditDialog("session", "turn", "", 1, func(value string) tea.Cmd {
		calls++
		assert.Equal(t, "first\nsecond", value)
		return func() tea.Msg { return nil }
	}).(*pendingMessageEditDialog)
	d.SetSize(70, 20)
	_, _ = d.Update(tea.PasteMsg{Content: "first"})
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, _ = d.Update(tea.PasteMsg{Content: "second"})
	assert.Zero(t, calls, "content Enter inserts newline, never saves")
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	assert.True(t, d.ActionsFocused())
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	assert.Equal(t, "first\nsecond", d.input.Value())
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	assert.False(t, d.ActionsFocused())
	assert.True(t, d.input.Focused())
	x, y := familyActionCell(t, d, "Save")
	_, cmd := d.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.NotNil(t, cmd)
	assert.Equal(t, 1, calls)
	assert.Contains(t, ansi.Strip(d.View()), "Saving")
}

func TestPendingMessageEditBoundsAndRepeatedView(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {40, 12}, {20, 6}, {10, 4}} {
		d := NewPendingMessageEditDialog("session", "turn", strings.Repeat("long 界 text\n", 60), 1, nil).(*pendingMessageEditDialog)
		d.SetSize(size[0], size[1])
		line, column, offset := d.input.Line(), d.input.Column(), d.input.ScrollYOffset()
		generation := d.bodyScroll.VisualGeneration()
		registrations := len(d.scrollviews)
		view := d.View()
		assert.LessOrEqual(t, lipgloss.Width(view), size[0])
		assert.LessOrEqual(t, lipgloss.Height(view), size[1])
		for range 4 {
			assert.Equal(t, view, d.View())
			_, _ = d.Position()
		}
		assert.Equal(t, line, d.input.Line())
		assert.Equal(t, column, d.input.Column())
		assert.Equal(t, offset, d.input.ScrollYOffset())
		assert.Equal(t, generation, d.bodyScroll.VisualGeneration())
		assert.Len(t, d.scrollviews, registrations)
	}
}

func TestPendingMessageEditWarmTheme(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	d := NewPendingMessageEditDialog("session", "turn", "text", 1, nil)
	d.SetSize(70, 20)
	before := d.View()
	changed := *original
	changed.Colors.BackgroundAlt = "#123456"
	changed.Colors.TextPrimary = "#abcdef"
	styles.ApplyTheme(&changed)
	after := d.View()
	assert.NotEqual(t, before, after)
	assert.Equal(t, ansi.Strip(before), ansi.Strip(after))
}

func TestPendingMessageEditMousePlacesMultilineCursor(t *testing.T) {
	d := NewPendingMessageEditDialog("session", "turn", "first\n界e\u0301second", 1, nil).(*pendingMessageEditDialog)
	d.SetSize(80, 20)
	_, _ = d.Update(tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	x, y, _, _ := d.BodyScrollBounds()
	_, _ = d.Update(tea.MouseClickMsg{X: x + 2, Y: y + 1, Button: tea.MouseLeft})
	assert.Equal(t, 1, d.input.Line())
	assert.Equal(t, 1, d.input.Column(), "second cell boundary follows the wide glyph")
	_, _ = d.Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	assert.Equal(t, "first\n界!e\u0301second", d.input.Value())
}

func TestPendingMessageEditInflightCancelTargetsOnlyOwner(t *testing.T) {
	d := NewPendingMessageEditDialog("session", "turn", "draft", 1, func(string) tea.Cmd {
		return func() tea.Msg { return PendingMessageSaveResultMsg{SessionID: "session", TurnID: "turn", EditorID: 1} }
	}).(*pendingMessageEditDialog)
	d.SetSize(70, 20)
	_, save := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	_, cancel := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cancel)
	closeMsg, ok := cancel().(CloseDialogByModelMsg)
	require.True(t, ok)
	assert.Same(t, d, closeMsg.Model)
	_, late := d.Update(save())
	assert.Nil(t, late, "an accepted backend write may finish but cannot reopen or close a different editor")
}

func TestPendingMessageEditBuriedSaveClosesOnlyOwner(t *testing.T) {
	mgr := New(animation.NewRuntime()).(*manager)
	mgr.SetSize(80, 24)
	t.Cleanup(mgr.Cleanup)
	editor := NewPendingMessageEditDialog("session", "turn", "draft", 21, func(string) tea.Cmd {
		return func() tea.Msg { return PendingMessageSaveResultMsg{SessionID: "session", TurnID: "turn", EditorID: 21} }
	}).(*pendingMessageEditDialog)
	_, _ = mgr.Update(OpenDialogMsg{Model: editor})
	_, save := editor.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	top := NewExitConfirmationDialog()
	_, _ = mgr.Update(OpenDialogMsg{Model: top})
	_, closeCmd := mgr.Update(save())
	for _, msg := range collectMsgs(closeCmd) {
		_, _ = mgr.Update(msg)
	}
	assert.Same(t, top, mgr.TopDialog())
	assert.False(t, mgr.HasDialog(func(d Dialog) bool { return d == editor }))
	_, duplicate := mgr.Update(PendingMessageSaveResultMsg{SessionID: "session", TurnID: "turn", EditorID: 21})
	assert.Nil(t, duplicate)
	assert.Same(t, top, mgr.TopDialog())
}

func TestPendingMessageEditCloseClickNeverDispatchesSave(t *testing.T) {
	calls := 0
	d := NewPendingMessageEditDialog("session", "turn", "draft", 1, func(string) tea.Cmd { calls++; return nil }).(*pendingMessageEditDialog)
	d.SetSize(70, 20)
	view := d.View()
	row, col := d.Position()
	dl := NewDialogLayout(view, row, col)
	for y := row; y < row+lipgloss.Height(view); y++ {
		for x := col; x < col+lipgloss.Width(view); x++ {
			click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
			if !d.CloseButtonHit(click, dl) {
				continue
			}
			_, cmd := d.Update(click)
			require.NotNil(t, cmd)
			closeMsg, ok := cmd().(CloseDialogByModelMsg)
			require.True(t, ok)
			assert.Same(t, d, closeMsg.Model)
			assert.Zero(t, calls)
			assert.Equal(t, "draft", d.input.Value())
			return
		}
	}
	t.Fatal("close control must remain clickable")
}

func TestPendingMessageEditUnsupportedCallbackRetainsDraft(t *testing.T) {
	d := NewPendingMessageEditDialog("session", "turn", "draft", 1, nil).(*pendingMessageEditDialog)
	d.SetSize(70, 20)
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModCtrl})
	assert.Nil(t, cmd)
	assert.Contains(t, d.saveError, "not supported")
	assert.False(t, d.closed)
	assert.Equal(t, "draft", d.input.Value())
}

func TestPendingMessageEditHitTestUsesScrolledGraphemeGeometry(t *testing.T) {
	d := NewPendingMessageEditDialog("session", "turn", "first\nsecond\n界e\u0301👩‍💻tail", 1, nil).(*pendingMessageEditDialog)
	d.input.SetWidth(20)
	d.input.SetHeight(1)
	d.input.MoveToEnd()
	require.Equal(t, 2, d.input.ScrollYOffset())
	value := d.input.Value()
	// Cell 4 is inside the two-cell emoji; snap before the entire cluster.
	d.placeCursor(4, 0)
	require.Equal(t, 2, d.input.Line())
	require.Equal(t, 3, d.input.Column())
	d.input, _ = d.input.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	require.Equal(t, "👩‍💻", d.input.SelectedText())
	before := d.input.Layout()
	for range 3 {
		_ = d.input.View()
		require.Equal(t, before, d.input.Layout())
	}
	require.Equal(t, value, d.input.Value())
	d.placeCursor(2, 0)
	require.Equal(t, 1, d.input.Column(), "wide glyph ends at cell two")
	require.Empty(t, d.input.SelectedText(), "click starts a new cursor placement")
}
