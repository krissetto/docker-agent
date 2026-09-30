package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
	"github.com/stretchr/testify/require"
	"os"
	"strings"
	"testing"
)

func settingsFromRoot(t *testing.T, root *appModel) dialog.Dialog {
	t.Helper()
	_, cmd := root.handleOpenSettingsDialog()
	opened, ok := firstOfType[dialog.OpenDialogMsg](collectMsgs(cmd))
	require.True(t, ok)
	opened.Model.SetSize(120, 40)
	return opened.Model
}
func settingsToggleRendered(t *testing.T, d dialog.Dialog, label string) {
	t.Helper()
	view := d.View()
	row, col := d.Position()
	for y, line := range strings.Split(ansi.Strip(view), "\n") {
		if before, _, ok := strings.Cut(line, label); ok {
			_, cmd := d.Update(tea.MouseClickMsg{X: col + ansi.StringWidth(before), Y: row + y, Button: tea.MouseLeft})
			require.Nil(t, cmd)
			return
		}
	}
	t.Fatalf("label missing: %s\n%s", label, ansi.Strip(view))
}
func TestSettingsActualDraftSaveReopenRestart(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 40)
	require.True(t, root.transparentBackground)
	require.True(t, root.dimInactivePanes)
	for _, enabled := range []bool{false, true} {
		d := settingsFromRoot(t, root)
		settingsToggleRendered(t, d, "Transparent background")
		settingsToggleRendered(t, d, "Dim inactive panes")
		_, cmd := d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
		applied, ok := firstOfType[messages.ApplySettingsMsg](collectMsgs(cmd))
		require.True(t, ok)
		require.Equal(t, enabled, applied.Preferences.TransparentBackground)
		require.Equal(t, enabled, applied.Preferences.DimInactivePanes)
		root.handleApplySettings(applied)
		require.Equal(t, enabled, userconfig.Get().GetTransparentBackground())
		require.Equal(t, enabled, userconfig.Get().GetDimInactivePanes())
		root, _, _ = frozenClockRoot(t, 120, 40)
		require.Equal(t, enabled, root.transparentBackground)
		require.Equal(t, enabled, root.dimInactivePanes)
		d = settingsFromRoot(t, root)
		for _, line := range strings.Split(ansi.Strip(d.View()), "\n") {
			if strings.Contains(line, "Transparent background") || strings.Contains(line, "Dim inactive panes") {
				if enabled {
					require.Contains(t, line, "[x]")
				} else {
					require.Contains(t, line, "[ ]")
				}
			}
		}
	}
}

func TestSettingsActualDraftPreservesEveryPreferenceAndIndependentThemeSave(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 40)
	want := messages.Preferences{
		Layout:   messages.LayoutSettings{SidebarPosition: messages.SidebarLeft, SectionSpacing: messages.SpacingRelaxed, SidebarInfoMode: messages.InfoModeDetailed, ActiveAgentsOnly: true, HideSessionPath: true, HideUsage: true, HideAgents: true, HideTools: true, HideTodos: true},
		Panel:    messages.PanelSettings{Elements: []messages.PanelElement{messages.PanelTodos, messages.PanelWorkspace}},
		SendMode: messages.SendModeSteer, SplitDiffView: false, ExpandThinking: true, HideToolResults: true, RenderImages: false, ShowBanner: false,
		DimInactivePanes: true, TransparentBackground: true, YOLO: true, RestoreTabs: true, Snapshot: true, CacheStablePrompts: true, WarnOnCacheMiss: true, Lean: true, TabTitleMaxLength: 43, Sound: true, SoundThreshold: 27, InterruptConfirmation: messages.InterruptModeNone,
	}
	root.handleApplySettings(messages.ApplySettingsMsg{Preferences: want})
	d := settingsFromRoot(t, root)
	settingsToggleRendered(t, d, "Transparent background")
	require.NoError(t, styles.SaveThemeToUserConfig("nord"))
	_, cmd := d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	applied, ok := firstOfType[messages.ApplySettingsMsg](collectMsgs(cmd))
	require.True(t, ok)
	want.TransparentBackground = false
	require.True(t, want.Equal(applied.Preferences), "real draft retains every persisted field")
	_, saveCmd := root.handleApplySettings(applied)
	notice, ok := firstOfType[notification.ShowMsg](collectMsgs(saveCmd))
	require.True(t, ok)
	require.Equal(t, notification.TypeSuccess, notice.Type)
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.Equal(t, "nord", cfg.Settings.Theme)
	root, _, _ = frozenClockRoot(t, 120, 40)
	root.panelSettings = panelSettingsFromConfig(cfg.Settings.GetPanel())
	d = settingsFromRoot(t, root)
	settingsToggleRendered(t, d, "Transparent background")
	_, cmd = d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	reopened, ok := firstOfType[messages.ApplySettingsMsg](collectMsgs(cmd))
	require.True(t, ok)
	want.TransparentBackground = true
	require.True(t, want.Equal(reopened.Preferences), "all fields survive fresh New and reopened draft")
}

func TestSettingsCancelAndFailedSaveAreNeverReportedAsPersisted(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 40)
	require.NoError(t, saveTestSettings(root.layoutSettings, root.sendMode))
	before, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	d := settingsFromRoot(t, root)
	settingsToggleRendered(t, d, "Dim inactive panes")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.False(t, hasMsg[messages.ApplySettingsMsg](collectMsgs(cmd)))
	after, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	require.Equal(t, before, after)
	d = settingsFromRoot(t, root)
	settingsToggleRendered(t, d, "Transparent background")
	_, cmd = d.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	applied, ok := firstOfType[messages.ApplySettingsMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.NoError(t, os.Remove(userconfig.Path()))
	require.NoError(t, os.Mkdir(userconfig.Path(), 0o700))
	_, cmd = root.handleApplySettings(applied)
	notice, ok := firstOfType[notification.ShowMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.NotEqual(t, notification.TypeSuccess, notice.Type)
	require.Contains(t, notice.Text, "could not be saved")
	require.DirExists(t, userconfig.Path())
}
