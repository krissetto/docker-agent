package tui

import (
	"bytes"
	"os"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
	"github.com/stretchr/testify/require"
)

func TestSettingsMergeIndependentLayoutFieldsAndPreservesExtras(t *testing.T) {
	setupSettingsConfigTest(t)
	require.NoError(t, userconfig.Update(func(c *userconfig.Config) error {
		c.Settings = &userconfig.Settings{Theme: "nord", ThemeLight: "gruvbox-light"}
		return nil
	}))
	original := preferencesFromConfig(userconfig.Get())
	a, b := original, original
	a.Layout.SidebarPosition = messages.SidebarLeft
	a.TransparentBackground = false
	b.Layout.HideUsage = true
	b.DimInactivePanes = false
	require.NoError(t, saveChangedPreferences(original, a))
	require.NoError(t, saveChangedPreferences(original, b))
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.Equal(t, "nord", cfg.Settings.Theme)
	require.Equal(t, "gruvbox-light", cfg.Settings.ThemeLight)
	require.Equal(t, messages.SidebarLeft, layoutSettingsFromConfig(cfg.Settings.GetLayout()).SidebarPosition)
	require.True(t, cfg.Settings.GetLayout().HideUsage)
	require.False(t, cfg.Settings.GetTransparentBackground())
	require.False(t, cfg.Settings.GetDimInactivePanes())
}

func settingsPicker(t *testing.T, root *appModel) dialog.Dialog {
	t.Helper()
	_, cmd := root.openThemePicker(root.settingsTransaction.id)
	open, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	open.Model.SetSize(120, 50)
	root.updateDialogCmd(open)
	return open.Model
}
func settingsThemeInput(t *testing.T, root *appModel, picker dialog.Dialog, msg tea.Msg) []tea.Msg {
	t.Helper()
	_, cmd := picker.Update(msg)
	events := collectMsgs(cmd)
	for _, event := range events {
		switch event.(type) {
		case messages.ThemePreviewMsg, messages.ChangeThemeMsg, messages.ThemeCancelPreviewMsg:
			root.handleSettingsTheme(event)
		}
	}
	return events
}
func chooseSettingsTheme(t *testing.T, root *appModel, picker dialog.Dialog, ref string) {
	t.Helper()
	settingsThemeInput(t, root, picker, tea.PasteMsg{Content: ref})
	settingsThemeInput(t, root, picker, tea.KeyPressMsg{Code: tea.KeyEnter})
}

func TestSettingsThemeNestedCancelOuterCancelAndAtomicSave(t *testing.T) {
	setupSettingsConfigTest(t)
	originalTheme, originalAuto := styles.CurrentTheme(), styles.AutoThemeEnabled()
	t.Cleanup(func() { styles.SetAutoThemeEnabled(originalAuto); styles.ApplyTheme(originalTheme) })
	styles.SetAutoThemeEnabled(false)
	styles.ApplyThemeRef("nord")
	root, _, _ := frozenClockRoot(t, 120, 50)
	require.NoError(t, styles.SaveThemeToUserConfig("gruvbox-dark"))
	before, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	actual := styles.CurrentTheme()
	d := settingsFromRoot(t, root)
	picker := settingsPicker(t, root)
	previews := settingsThemeInput(t, root, picker, tea.PasteMsg{Content: "dracula"})
	require.Equal(t, "dracula", styles.CurrentTheme().Ref)
	settingsThemeInput(t, root, picker, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Same(t, actual, styles.CurrentTheme())
	for _, event := range previews {
		root.handleSettingsTheme(event)
	}
	require.Same(t, actual, styles.CurrentTheme(), "stale nested previews cannot revive a closed picker")
	picker = settingsPicker(t, root)
	chooseSettingsTheme(t, root, picker, "dracula")
	after, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, "dracula", styles.CurrentTheme().Ref)
	settingsCancel(t, root, d)
	require.Same(t, actual, styles.CurrentTheme())
	d = settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Transparent background")
	picker = settingsPicker(t, root)
	chooseSettingsTheme(t, root, picker, "dracula")
	settingsSave(t, root, d)
	require.Equal(t, "dracula", userconfig.Get().Theme)
	require.False(t, userconfig.Get().GetTransparentBackground())
}

func TestSettingsThemeFailureKeepsDraftAndCancelRestoresAuto(t *testing.T) {
	setupSettingsConfigTest(t)
	originalTheme, originalAuto := styles.CurrentTheme(), styles.AutoThemeEnabled()
	t.Cleanup(func() { styles.SetAutoThemeEnabled(originalAuto); styles.ApplyTheme(originalTheme) })
	styles.ApplyThemeRef("nord")
	styles.SetAutoThemeEnabled(true)
	root, _, _ := frozenClockRoot(t, 120, 50)
	actual := styles.CurrentTheme()
	d := settingsFromRoot(t, root)
	picker := settingsPicker(t, root)
	chooseSettingsTheme(t, root, picker, "dracula")
	require.False(t, styles.AutoThemeEnabled())
	require.NoError(t, os.MkdirAll(userconfig.Path(), 0o700))
	settingsSave(t, root, d)
	require.NotNil(t, root.settingsTransaction)
	require.Equal(t, "dracula", styles.CurrentTheme().Ref)
	settingsCancel(t, root, d)
	require.Same(t, actual, styles.CurrentTheme())
	require.True(t, styles.AutoThemeEnabled())
}

func TestSettingsNonvisualDraftWaitsForSuccessfulSave(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	d := settingsFromRoot(t, root)
	// Controls -> categories, choose Behavior, enter controls.
	d.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	old := root.sendMode
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	require.Nil(t, cmd)
	require.Equal(t, old, root.sendMode)
	require.Empty(t, userconfig.Get().GetBusySendMode())
	require.NoError(t, os.MkdirAll(userconfig.Path(), 0o700))
	settingsSave(t, root, d)
	require.Equal(t, old, root.sendMode)
	require.NoError(t, os.Remove(userconfig.Path()))
	settingsSave(t, root, d)
	require.NotEqual(t, old, root.sendMode)
	require.Equal(t, string(root.sendMode), userconfig.Get().GetBusySendMode())
}

func TestSettingsNestedPickerForcedCleanupRestoresEntryAndOuterTheme(t *testing.T) {
	setupSettingsConfigTest(t)
	initial, auto := styles.CurrentTheme(), styles.AutoThemeEnabled()
	t.Cleanup(func() { styles.SetAutoThemeEnabled(auto); styles.ApplyTheme(initial) })
	styles.SetAutoThemeEnabled(false)
	styles.ApplyThemeRef("nord")
	root, _, _ := frozenClockRoot(t, 120, 50)
	outer := styles.CurrentTheme()
	d := settingsFromRoot(t, root)
	picker := settingsPicker(t, root)
	chooseSettingsTheme(t, root, picker, "dracula")
	entry := styles.CurrentTheme()
	picker = settingsPicker(t, root)
	settingsThemeInput(t, root, picker, tea.PasteMsg{Content: "gruvbox-dark"})
	require.NotSame(t, entry, styles.CurrentTheme())
	dialog.CleanupDialog(picker)
	root.reconcileSettings()
	require.Same(t, entry, styles.CurrentTheme())
	require.Nil(t, root.settingsTransaction.picker)
	picker = settingsPicker(t, root)
	events := settingsThemeInput(t, root, picker, tea.PasteMsg{Content: "gruvbox-dark"})
	root.updateDialogCmd(dialog.CloseAllDialogsMsg{})
	require.Same(t, outer, styles.CurrentTheme())
	require.Nil(t, root.settingsTransaction)
	for _, event := range events {
		root.handleSettingsTheme(event)
	}
	require.Same(t, outer, styles.CurrentTheme())
	require.True(t, d.(dialog.SettingsEditor).Disposed())
}

func TestSettingsRollbackRestoresRawUnsupportedImagePreference(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	writer := tuiimage.NewWriter(&bytes.Buffer{})
	writer.SetSupported(false)
	writer.SetEnabled(true)
	root.imageWriter = writer
	d := settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Render images")
	require.False(t, writer.Enabled())
	require.False(t, writer.RenderingEnabled())
	settingsCancel(t, root, d)
	require.True(t, writer.Enabled())
	require.False(t, writer.RenderingEnabled())
}

func TestSettingsShutdownRollsBackThemeAndInvalidatesDraft(t *testing.T) {
	setupSettingsConfigTest(t)
	initial, auto := styles.CurrentTheme(), styles.AutoThemeEnabled()
	t.Cleanup(func() { styles.SetAutoThemeEnabled(auto); styles.ApplyTheme(initial) })
	styles.SetAutoThemeEnabled(false)
	styles.ApplyThemeRef("nord")
	root, _, _ := frozenClockRoot(t, 120, 50)
	actual := styles.CurrentTheme()
	d := settingsFromRoot(t, root)
	picker := settingsPicker(t, root)
	settingsThemeInput(t, root, picker, tea.PasteMsg{Content: "dracula"})
	root.Shutdown()
	require.Nil(t, root.settingsTransaction)
	require.Same(t, actual, styles.CurrentTheme())
	require.True(t, d.(dialog.SettingsEditor).Disposed())
}
