package tui

import (
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
	"github.com/stretchr/testify/require"
)

func settingsFromRoot(t *testing.T, root *appModel) dialog.Dialog {
	t.Helper()
	_, cmd := root.handleOpenSettingsDialog()
	opened, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	opened.Model.SetSize(120, 50)
	root.updateDialogCmd(opened)
	return opened.Model
}
func settingsClickRendered(t *testing.T, d dialog.Dialog, label string) tea.Cmd {
	t.Helper()
	view := d.View()
	row, col := d.Position()
	lines := strings.Split(ansi.Strip(view), "\n")
	for y := len(lines) - 1; y >= 0; y-- {
		line := lines[y]
		if before, _, ok := strings.Cut(line, label); ok {
			_, cmd := d.Update(tea.MouseClickMsg{X: col + ansi.StringWidth(before), Y: row + y, Button: tea.MouseLeft})
			return cmd
		}
	}
	t.Fatalf("label missing: %s\n%s", label, ansi.Strip(view))
	return nil
}
func settingsToggleRendered(t *testing.T, d dialog.Dialog, label string) tea.Cmd {
	return settingsClickRendered(t, d, label)
}
func settingsPreview(t *testing.T, root *appModel, d dialog.Dialog, label string) messages.PreviewSettingsMsg {
	t.Helper()
	preview, ok := firstOfType[messages.PreviewSettingsMsg](collectMsgs(settingsToggleRendered(t, d, label)))
	require.True(t, ok)
	root.handleSettingsPreview(preview)
	return preview
}
func settingsSave(t *testing.T, root *appModel, d dialog.Dialog) tea.Cmd {
	t.Helper()
	msg, ok := firstOfType[messages.ApplySettingsMsg](collectMsgs(settingsClickRendered(t, d, "Save")))
	require.True(t, ok)
	_, cmd := root.handleApplySettings(msg)
	return cmd
}
func settingsCancel(t *testing.T, root *appModel, d dialog.Dialog) {
	t.Helper()
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	msg, ok := firstOfType[messages.CancelSettingsMsg](collectMsgs(cmd))
	require.True(t, ok)
	root.handleSettingsCancel(msg)
}

func TestSettingsActualDraftSaveReopenRestart(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	for _, enabled := range []bool{false, true} {
		d := settingsFromRoot(t, root)
		settingsPreview(t, root, d, "Transparent background")
		settingsPreview(t, root, d, "Dim inactive panes")
		require.Equal(t, enabled, root.transparentBackground)
		require.Equal(t, enabled, root.dimInactivePanes)
		require.Equal(t, !enabled, userconfig.Get().GetTransparentBackground(), "preview must not persist")
		_, cmd := d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
		require.Nil(t, cmd)
		notice, ok := firstOfType[notification.ShowMsg](collectMsgs(settingsSave(t, root, d)))
		require.True(t, ok)
		require.Equal(t, "Settings saved", notice.Text)
		require.Equal(t, enabled, userconfig.Get().GetTransparentBackground())
		require.Equal(t, enabled, userconfig.Get().GetDimInactivePanes())
		root, _, _ = frozenClockRoot(t, 120, 50)
		require.Equal(t, enabled, root.transparentBackground)
		require.Equal(t, enabled, root.dimInactivePanes)
	}
}

func TestSettingsActualDraftPreservesUneditedConcurrentFields(t *testing.T) {
	setupSettingsConfigTest(t)
	a, _, _ := frozenClockRoot(t, 120, 50)
	b, _, _ := frozenClockRoot(t, 120, 50)
	da, db := settingsFromRoot(t, a), settingsFromRoot(t, b)
	settingsPreview(t, a, da, "Transparent background")
	settingsPreview(t, a, da, "Dim inactive panes")
	settingsSave(t, a, da)
	settingsPreview(t, b, db, "Show startup banner")
	require.NoError(t, styles.SaveThemeToUserConfig("nord"))
	settingsSave(t, b, db)
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.False(t, cfg.Settings.GetTransparentBackground())
	require.False(t, cfg.Settings.GetDimInactivePanes())
	require.False(t, cfg.Settings.GetShowBanner())
	require.Equal(t, "nord", cfg.Settings.Theme)
}

func TestSettingsCancelRestoresActualRuntimeAndRejectsLatePreview(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	require.NoError(t, saveTestSettings(root.layoutSettings, root.sendMode))
	before, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	root.dimInactivePanes = false
	root.sessionState.SetSplitDiffView(false)
	root.sessionStates["different"] = &service.SessionState{}
	root.sessionStates["different"].SetExpandThinking(true)
	d := settingsFromRoot(t, root)
	stale := settingsPreview(t, root, d, "Dim inactive panes")
	settingsPreview(t, root, d, "Split diff view")
	settingsPreview(t, root, d, "Expand thinking by default")
	settingsCancel(t, root, d)
	require.False(t, root.dimInactivePanes)
	require.False(t, root.sessionState.SplitDiffView())
	require.True(t, root.sessionStates["different"].ExpandThinking())
	root.handleSettingsPreview(stale)
	require.False(t, root.dimInactivePanes)
	after, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Nil(t, root.settingsTransaction)
}

func TestSettingsFailedSaveKeepsDraftPreviewAndCanRetry(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	require.NoError(t, saveTestSettings(root.layoutSettings, root.sendMode))
	before, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	d := settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Transparent background")
	require.NoError(t, os.Remove(userconfig.Path()))
	require.NoError(t, os.Mkdir(userconfig.Path(), 0o700))
	notice, ok := firstOfType[notification.ShowMsg](collectMsgs(settingsSave(t, root, d)))
	require.True(t, ok)
	require.Equal(t, notification.TypeWarning, notice.Type)
	require.Contains(t, notice.Text, "could not be saved")
	require.NotNil(t, root.settingsTransaction)
	require.False(t, d.(dialog.SettingsEditor).Disposed())
	require.False(t, root.transparentBackground)
	require.Contains(t, ansi.Strip(d.View()), "Save")
	require.NoError(t, os.Remove(userconfig.Path()))
	require.NoError(t, os.WriteFile(userconfig.Path(), before, 0o600))
	settingsSave(t, root, d)
	require.Nil(t, root.settingsTransaction)
	require.False(t, userconfig.Get().GetTransparentBackground())
}

func TestSettingsForcedCleanupAndReorderedPreviews(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	d := settingsFromRoot(t, root)
	first, ok := firstOfType[messages.PreviewSettingsMsg](collectMsgs(settingsToggleRendered(t, d, "Transparent background")))
	require.True(t, ok)
	second, ok := firstOfType[messages.PreviewSettingsMsg](collectMsgs(settingsToggleRendered(t, d, "Dim inactive panes")))
	require.True(t, ok)
	root.handleSettingsPreview(second)
	root.handleSettingsPreview(first)
	require.False(t, root.transparentBackground)
	require.False(t, root.dimInactivePanes)
	root.updateDialogCmd(dialog.CloseAllDialogsMsg{})
	require.True(t, root.transparentBackground)
	require.True(t, root.dimInactivePanes)
	require.Nil(t, root.settingsTransaction)
	root.handleSettingsPreview(second)
	require.True(t, root.dimInactivePanes)
	d = settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Transparent background")
	root.dialogMgr.Cleanup()
	root.reconcileSettings()
	require.True(t, root.transparentBackground)
}

func TestSettingsNoChangeSaveDoesNotWrite(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	d := settingsFromRoot(t, root)
	settingsSave(t, root, d)
	_, err := os.Stat(userconfig.Path())
	require.True(t, os.IsNotExist(err))
	require.Nil(t, root.settingsTransaction)
}

func saveSettingsDraftForTest(t *testing.T, root *appModel, prefs messages.Preferences) {
	t.Helper()
	_, cmd := root.handleOpenSettingsDialog()
	_, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	tx := root.settingsTransaction
	// Build the submitted draft through its constructor normalization and actual Save action.
	d := dialog.NewSettingsDialog(prefs, true, tx.id).(dialog.SettingsEditor)
	d.SetSize(120, 50)
	tx.editor = d
	msg, ok := firstOfType[messages.ApplySettingsMsg](collectMsgs(settingsClickRendered(t, d, "Save")))
	require.True(t, ok)
	root.handleApplySettings(msg)
}
