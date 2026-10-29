package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestComposerAlignmentSettingPreviewCancelSaveReloadAndClear(t *testing.T) {
	setupSettingsConfigTest(t)
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Layout: &userconfig.LayoutSettings{
			SidebarPosition: "bottom",
			HideUsage:       true,
		}}
		return nil
	}))

	root, _, _ := frozenClockRoot(t, 120, 50)
	root.hideSidebar = false
	d := settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Align input with messages")
	require.True(t, root.layoutSettings.AlignInputWithMessages, "toggle previews the layout")
	require.False(t, userconfig.Get().GetLayout().AlignInputWithMessages, "preview is not persisted")
	settingsCancel(t, root, d)
	require.False(t, root.layoutSettings.AlignInputWithMessages, "cancel rolls back the layout")
	require.False(t, userconfig.Get().GetLayout().AlignInputWithMessages)

	d = settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Align input with messages")
	settingsSave(t, root, d)
	layout := userconfig.Get().GetLayout()
	require.True(t, layout.AlignInputWithMessages)
	require.Equal(t, "bottom", layout.SidebarPosition)
	require.True(t, layout.HideUsage)

	root, _, _ = frozenClockRoot(t, 120, 50)
	root.hideSidebar = false
	require.True(t, root.layoutSettings.AlignInputWithMessages, "a new TUI loads the saved alignment")
	d = settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Align input with messages")
	settingsSave(t, root, d)
	layout = userconfig.Get().GetLayout()
	require.False(t, layout.AlignInputWithMessages, "turning it off clears the persisted opt-in")
	require.Equal(t, "bottom", layout.SidebarPosition, "clearing preserves other layout fields")
	require.True(t, layout.HideUsage, "clearing preserves other layout fields")
	require.Equal(t, messages.SidebarBottom, root.layoutSettings.SidebarPosition)
}

func TestComposerAlignmentSettingSparseMergePreservesConcurrentLayoutChanges(t *testing.T) {
	setupSettingsConfigTest(t)
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Layout: &userconfig.LayoutSettings{SectionSpacing: "compact"}}
		return nil
	}))

	original := preferencesFromConfig(userconfig.Get())
	draft := original
	draft.Layout.AlignInputWithMessages = true
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings.Layout.SidebarPosition = "bottom"
		cfg.Settings.Layout.HideUsage = true
		return nil
	}))

	require.NoError(t, saveChangedPreferences(original, draft))
	layout := userconfig.Get().GetLayout()
	require.True(t, layout.AlignInputWithMessages)
	require.Equal(t, "bottom", layout.SidebarPosition, "an unedited concurrent field is retained")
	require.True(t, layout.HideUsage, "an unedited concurrent field is retained")
	require.Equal(t, "compact", layout.SectionSpacing, "pre-existing layout fields are retained")
}

func TestComposerAlignmentSettingClearsLastLayoutOptIn(t *testing.T) {
	setupSettingsConfigTest(t)
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Layout: &userconfig.LayoutSettings{AlignInputWithMessages: true}}
		return nil
	}))

	original := preferencesFromConfig(userconfig.Get())
	draft := original
	draft.Layout.AlignInputWithMessages = false
	require.NoError(t, saveChangedPreferences(original, draft))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.Nil(t, cfg.Settings.Layout, "the empty layout entry is removed after clearing the last opt-in")
}

func TestVisibleRepliesPersistenceDefaultsAndRoundTrip(t *testing.T) {
	setupSettingsConfigTest(t)
	require.Equal(t, messages.VisibleRepliesThree, preferencesFromConfig(userconfig.Get()).VisibleReplies)

	prefs := preferencesFromConfig(userconfig.Get())
	prefs.VisibleReplies = messages.VisibleRepliesFive
	require.NoError(t, savePreferences(prefs))
	require.Equal(t, "5", userconfig.Get().GetVisibleReplies())

	root, _, _ := frozenClockRoot(t, 120, 50)
	require.Equal(t, messages.VisibleRepliesFive, preferencesFromConfig(userconfig.Get()).VisibleReplies)
	d := settingsFromRoot(t, root)
	settingsClickRendered(t, d, "Visible replies")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	preview, ok := firstOfType[messages.PreviewSettingsMsg](collectMsgs(cmd))
	require.True(t, ok)
	require.Equal(t, messages.VisibleRepliesAll, preview.Preferences.VisibleReplies)
	root.handleSettingsPreview(preview)
	settingsCancel(t, root, d)
	require.Equal(t, "5", userconfig.Get().GetVisibleReplies(), "preview/cancel do not persist")

	prefs = preferencesFromConfig(userconfig.Get())
	prefs.VisibleReplies = messages.VisibleRepliesThree
	require.NoError(t, savePreferences(prefs))
	require.Empty(t, userconfig.Get().VisibleReplies, "the default value is omitted")
}

func TestVisibleRepliesSparseSavePreservesConcurrentSetting(t *testing.T) {
	setupSettingsConfigTest(t)
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{VisibleReplies: "5"}
		return nil
	}))
	original := preferencesFromConfig(userconfig.Get())
	draft := original
	draft.VisibleReplies = messages.VisibleRepliesAll
	concurrent := original
	concurrent.HideToolResults = true
	require.NoError(t, saveChangedPreferences(original, concurrent))
	require.NoError(t, saveChangedPreferences(original, draft))
	require.Equal(t, "all", userconfig.Get().GetVisibleReplies())
	require.True(t, userconfig.Get().HideToolResults, "an unrelated concurrent setting is retained")
}
