package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/dialog"
)

// applyOpenDialogMsgs feeds every dialog.OpenDialogMsg in cmd back into the
// model, so the dialog manager actually receives them. This mirrors how the
// real bubbletea event loop drains commands.
func applyOpenDialogMsgs(t *testing.T, m *appModel, cmd tea.Cmd) {
	t.Helper()
	for _, msg := range collectMsgs(cmd) {
		if open, ok := msg.(dialog.OpenDialogMsg); ok {
			_, _ = m.Update(open)
		}
	}
}

func TestCtrlC_NoDialog_OpensExitConfirmation(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	require.False(t, m.dialogMgr.Open(), "no dialog should be open initially")

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	applyOpenDialogMsgs(t, m, cmd)

	assert.True(t, m.dialogMgr.Open(), "ctrl+c should open a dialog")
	assert.True(t, m.dialogMgr.TopIsExitConfirmation(),
		"ctrl+c with no dialog should open the exit confirmation dialog")
}

func TestCtrlC_OnOtherDialog_CancelsDialog(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	_, _ = m.Update(dialog.OpenDialogMsg{Model: dialog.NewHelpDialog(nil)})
	require.True(t, m.dialogMgr.Open())

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	msgs := collectMsgs(cmd)
	assert.False(t, hasMsg[tea.QuitMsg](msgs))
	require.True(t, hasMsg[dialog.CloseDialogMsg](msgs))
	for _, msg := range msgs {
		_, _ = m.Update(msg)
	}
	assert.False(t, m.dialogMgr.Open())
}

func TestCtrlC_OnExitConfirmation_ForwardsAndExits(t *testing.T) {
	t.Parallel()

	m, _ := newTestModel(t)
	neutralizeExitFunc(t, m)

	// Put the exit confirmation dialog on top first (via a regular ctrl+c).
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	applyOpenDialogMsgs(t, m, cmd)
	require.True(t, m.dialogMgr.TopIsExitConfirmation(),
		"exit confirmation should be the topmost dialog")

	// A second ctrl+c is forwarded to the exit confirmation, which signals
	// ExitConfirmedMsg. Feed every produced message back through the model
	// so we end up at tea.Quit.
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	require.NotNil(t, cmd, "second ctrl+c should produce a command")

	sawQuit := false
	for _, msg := range collectMsgs(cmd) {
		if _, ok := msg.(dialog.ExitConfirmedMsg); ok {
			_, exitCmd := m.Update(msg)
			if hasMsg[tea.QuitMsg](collectMsgs(exitCmd)) {
				sawQuit = true
			}
		}
	}
	assert.True(t, sawQuit,
		"two ctrl+c presses must result in tea.Quit (exit the program)")
}
