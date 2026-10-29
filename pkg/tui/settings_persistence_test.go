package tui

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// setupSettingsConfigTest isolates the user config in a temp dir. Tests using
// it must not be parallel: the config dir override is process-global.
func setupSettingsConfigTest(t *testing.T) {
	t.Helper()
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
}

func saveTestSettings(layout messages.LayoutSettings, mode messages.SendMode) error {
	settings := userconfig.Get()
	return savePreferences(messages.Preferences{
		Layout: layout, SendMode: mode, SplitDiffView: settings.GetSplitDiffView(),
		GitDiffLayout:  messages.ParseGitDiffLayout(settings.GetGitDiffLayout()),
		ExpandThinking: settings.GetExpandThinking(), CompactWorkLog: settings.GetCompactWorkLog(), HideToolResults: settings.HideToolResults,
		RenderImages: settings.GetRenderImages(), ShowBanner: settings.GetShowBanner(),
		DimInactivePanes:      settings.GetDimInactivePanes(),
		TransparentBackground: settings.GetTransparentBackground(),
		YOLO:                  settings.YOLO, RestoreTabs: settings.GetRestoreTabs(), Snapshot: settings.SnapshotsEnabled(),
		CacheStablePrompts: settings.CacheStablePromptsEnabled(), WarnOnCacheMiss: settings.CacheMissWarningsEnabled(),
		Lean: settings.Lean, TabTitleMaxLength: settings.GetTabTitleMaxLength(),
		Sound: settings.GetSound(), SoundThreshold: settings.GetSoundThreshold(),
		InterruptConfirmation: messages.ParseInterruptMode(settings.GetInterruptConfirmation()),
	})
}

func TestLayoutSettingsFromConfig(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		messages.LayoutSettings{SidebarPosition: messages.SidebarRight, SectionSpacing: messages.SpacingNormal, SidebarInfoMode: messages.InfoModeCompact},
		layoutSettingsFromConfig(userconfig.LayoutSettings{}),
		"empty config falls back to the default position, spacing and info mode")

	assert.Equal(t,
		messages.LayoutSettings{SidebarPosition: messages.SidebarRight, SectionSpacing: messages.SpacingNormal, SidebarInfoMode: messages.InfoModeCompact},
		layoutSettingsFromConfig(userconfig.LayoutSettings{SidebarPosition: "bogus", SectionSpacing: "bogus", SidebarInfoMode: "bogus"}),
		"unknown values fall back to the defaults")

	got := layoutSettingsFromConfig(userconfig.LayoutSettings{
		SidebarPosition:  "bottom",
		SectionSpacing:   "compact",
		SidebarInfoMode:  "detailed",
		ActiveAgentsOnly: true,
		HideSessionPath:  true,
		HideUsage:        true,
		HideAgents:       true,
		HideTools:        true,
		HideTodos:        true,
	})
	assert.Equal(t, messages.LayoutSettings{
		SidebarPosition:  messages.SidebarBottom,
		SectionSpacing:   messages.SpacingCompact,
		SidebarInfoMode:  messages.InfoModeDetailed,
		ActiveAgentsOnly: true,
		HideSessionPath:  true,
		HideUsage:        true,
		HideAgents:       true,
		HideTools:        true,
		HideTodos:        true,
	}, got)
}

func TestSaveSettingsToUserConfig_RoundTrip(t *testing.T) {
	setupSettingsConfigTest(t)

	saved := messages.LayoutSettings{
		SidebarPosition: messages.SidebarLeft,
		SectionSpacing:  messages.SpacingRelaxed,
		SidebarInfoMode: messages.InfoModeCompact,
		HideSessionPath: true,
		HideTools:       true,
	}
	require.NoError(t, saveTestSettings(saved, messages.SendModeQueue))

	assert.Equal(t, saved, layoutSettingsFromConfig(userconfig.Get().GetLayout()))
	assert.Equal(t, messages.SendModeQueue, messages.ParseSendMode(userconfig.Get().GetBusySendMode()))
}

func TestSaveSettingsToUserConfig_DefaultsClearEntry(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, saveTestSettings(messages.LayoutSettings{
		SidebarPosition: messages.SidebarTop,
	}, messages.SendModeQueue))
	require.NoError(t, saveTestSettings(messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
	}, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.GetSettings().Layout, "default layout clears the config entry")
	assert.Empty(t, cfg.GetSettings().GetBusySendMode(), "the default send mode is not written out")
}

func TestSaveSettingsToUserConfig_OmitsDefaultPosition(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, saveTestSettings(messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		HideUsage:       true,
	}, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	layout := cfg.GetSettings().Layout
	require.NotNil(t, layout)
	assert.Empty(t, layout.SidebarPosition, "the default position is not written out")
	assert.Empty(t, layout.SectionSpacing, "the default spacing is not written out")
	assert.True(t, layout.HideUsage)
}

func TestSavePreferences_RoundTripAndPreservesExtra(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Extra: map[string]any{"future_setting": "kept"}}
		return nil
	}))

	preferences := messages.Preferences{
		Layout: messages.LayoutSettings{
			SidebarPosition:  messages.SidebarBottom,
			SectionSpacing:   messages.SpacingCompact,
			SidebarInfoMode:  messages.InfoModeDetailed,
			ActiveAgentsOnly: true,
			HideAgents:       true,
		},
		SendMode:           messages.SendModeQueue,
		SplitDiffView:      false,
		GitDiffLayout:      messages.GitDiffSplit,
		ExpandThinking:     true,
		HideToolResults:    true,
		ShowBanner:         false,
		YOLO:               true,
		RestoreTabs:        true,
		Snapshot:           true,
		CacheStablePrompts: true,
		WarnOnCacheMiss:    true,
		Lean:               true,
		TabTitleMaxLength:  42,
		Sound:              true,
		SoundThreshold:     17,
	}
	require.NoError(t, savePreferences(preferences))

	settings := userconfig.Get()
	assert.Equal(t, preferences.Layout, layoutSettingsFromConfig(settings.GetLayout()))
	assert.Equal(t, preferences.SendMode, messages.ParseSendMode(settings.GetBusySendMode()))
	assert.Equal(t, preferences.SplitDiffView, settings.GetSplitDiffView())
	assert.Equal(t, preferences.GitDiffLayout, messages.ParseGitDiffLayout(settings.GetGitDiffLayout()))
	assert.Equal(t, preferences.ExpandThinking, settings.GetExpandThinking())
	assert.Equal(t, preferences.HideToolResults, settings.HideToolResults)
	assert.Equal(t, preferences.ShowBanner, settings.GetShowBanner())
	assert.Equal(t, preferences.YOLO, settings.YOLO)
	assert.Equal(t, preferences.RestoreTabs, settings.GetRestoreTabs())
	assert.Equal(t, preferences.Snapshot, settings.SnapshotsEnabled())
	assert.Equal(t, preferences.CacheStablePrompts, settings.CacheStablePromptsEnabled())
	assert.Equal(t, preferences.WarnOnCacheMiss, settings.CacheMissWarningsEnabled())
	assert.Equal(t, preferences.Lean, settings.Lean)
	assert.Equal(t, preferences.TabTitleMaxLength, settings.GetTabTitleMaxLength())
	assert.Equal(t, preferences.Sound, settings.GetSound())
	assert.Equal(t, preferences.SoundThreshold, settings.GetSoundThreshold())
	assert.Equal(t, "kept", settings.Extra["future_setting"])
}

func TestSavePreferences_DefaultsClearEntries(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, savePreferences(messages.Preferences{
		Layout:             messages.LayoutSettings{SidebarPosition: messages.SidebarLeft},
		SendMode:           messages.SendModeSteer,
		SplitDiffView:      false,
		ExpandThinking:     true,
		RestoreTabs:        true,
		Snapshot:           true,
		CacheStablePrompts: true,
		WarnOnCacheMiss:    true,
		TabTitleMaxLength:  40,
		SoundThreshold:     40,
	}))
	require.NoError(t, savePreferences(messages.Preferences{
		Layout:            messages.LayoutSettings{SidebarPosition: messages.SidebarRight, SectionSpacing: messages.SpacingNormal},
		SendMode:          messages.SendModeSteer,
		SplitDiffView:     true,
		ShowBanner:        true,
		TabTitleMaxLength: userconfig.DefaultTabTitleMaxLength,
		SoundThreshold:    userconfig.DefaultSoundThreshold,
	}))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	settings := cfg.Settings
	require.NotNil(t, settings)
	assert.Nil(t, settings.Layout)
	assert.Empty(t, settings.BusySendMode)
	assert.Nil(t, settings.SplitDiffView)
	assert.Nil(t, settings.ShowBanner)
	assert.Nil(t, settings.ExpandThinking)
	assert.Nil(t, settings.RestoreTabs)
	assert.Nil(t, settings.Snapshot)
	assert.Nil(t, settings.CacheStablePrompts)
	assert.Nil(t, settings.WarnOnCacheMiss)
	assert.Zero(t, settings.TabTitleMaxLength)
	assert.Zero(t, settings.SoundThreshold)
}

func TestSaveSettingsToUserConfig_HideSessionPathKeepsEntry(t *testing.T) {
	setupSettingsConfigTest(t)

	saved := messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		SidebarInfoMode: messages.InfoModeCompact,
		HideSessionPath: true,
	}
	require.NoError(t, saveTestSettings(saved, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	layout := cfg.GetSettings().Layout
	require.NotNil(t, layout, "hide_session_path alone must keep the layout entry")
	assert.True(t, layout.HideSessionPath)
	assert.Equal(t, saved, layoutSettingsFromConfig(userconfig.Get().GetLayout()))
}

func TestSavePreferences_DetailedInfoModeKeepsEntry(t *testing.T) {
	setupSettingsConfigTest(t)

	saved := messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		SidebarInfoMode: messages.InfoModeDetailed,
	}
	require.NoError(t, saveTestSettings(saved, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	layout := cfg.GetSettings().Layout
	require.NotNil(t, layout, "a detailed info mode alone must keep the layout entry")
	assert.Equal(t, "detailed", layout.SidebarInfoMode)
	assert.Empty(t, layout.SidebarPosition, "the default position is not written out")
	assert.Empty(t, layout.SectionSpacing, "the default spacing is not written out")
	assert.Equal(t, saved, layoutSettingsFromConfig(userconfig.Get().GetLayout()))
}

func TestSavePreferences_CompactInfoModeClearsEntry(t *testing.T) {
	setupSettingsConfigTest(t)

	require.NoError(t, saveTestSettings(messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		SidebarInfoMode: messages.InfoModeDetailed,
	}, messages.SendModeSteer))
	require.NoError(t, saveTestSettings(messages.LayoutSettings{
		SidebarPosition: messages.SidebarRight,
		SectionSpacing:  messages.SpacingNormal,
		SidebarInfoMode: messages.InfoModeCompact,
	}, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.GetSettings().Layout,
		"the default layout with an explicit compact info mode clears the config entry")
}

func TestSavePreferences_ActiveAgentsOnlyRoundTrip(t *testing.T) {
	setupSettingsConfigTest(t)

	saved := messages.LayoutSettings{
		SidebarPosition:  messages.SidebarRight,
		SectionSpacing:   messages.SpacingNormal,
		SidebarInfoMode:  messages.InfoModeCompact,
		ActiveAgentsOnly: true,
	}
	require.NoError(t, saveTestSettings(saved, messages.SendModeSteer))

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	layout := cfg.GetSettings().Layout
	require.NotNil(t, layout, "active_agents_only alone must keep the layout entry")
	assert.True(t, layout.ActiveAgentsOnly)
	assert.Empty(t, layout.SidebarPosition, "the default position is not written out")
	assert.Equal(t, saved, layoutSettingsFromConfig(userconfig.Get().GetLayout()))

	// Turning the filter back off restores the all-default layout and clears
	// the entry entirely.
	saved.ActiveAgentsOnly = false
	require.NoError(t, saveTestSettings(saved, messages.SendModeSteer))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.GetSettings().Layout, "the default (off) filter is not written out")
}

func TestDimInactivePanesPreferenceRoundTrip(t *testing.T) {
	setupSettingsConfigTest(t)
	require.True(t, userconfig.Get().GetDimInactivePanes())
	preferences := messages.Preferences{DimInactivePanes: false}
	require.NoError(t, savePreferences(preferences))
	require.False(t, userconfig.Get().GetDimInactivePanes())
	preferences.DimInactivePanes = true
	require.NoError(t, savePreferences(preferences))
	require.True(t, userconfig.Get().GetDimInactivePanes())
	require.Nil(t, userconfig.Get().DimInactivePanes)
}

func TestBusySendModeConfigDefaultsAndRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		want   messages.SendMode
		stored string
	}{
		{name: "new user", want: messages.SendModeSteer},
		{name: "unset", want: messages.SendModeSteer},
		{name: "unknown", raw: "bogus", want: messages.SendModeSteer},
		{name: "explicit steer", raw: "steer", want: messages.SendModeSteer},
		{name: "explicit queue", raw: "queue", want: messages.SendModeQueue, stored: "queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupSettingsConfigTest(t)
			if tc.name != "new user" {
				require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
					cfg.Settings = &userconfig.Settings{BusySendMode: tc.raw}
					return nil
				}))
			}
			mode := messages.ParseSendMode(userconfig.Get().GetBusySendMode())
			require.Equal(t, tc.want, mode)
			require.NoError(t, saveTestSettings(messages.LayoutSettings{}, mode))
			cfg, err := userconfig.Load()
			require.NoError(t, err)
			assert.Equal(t, tc.stored, cfg.GetSettings().GetBusySendMode())
			assert.Equal(t, tc.want, messages.ParseSendMode(cfg.GetSettings().GetBusySendMode()))
		})
	}
}

func TestTransparentBackgroundPreferenceRoundTrip(t *testing.T) {
	setupSettingsConfigTest(t)
	require.True(t, userconfig.Get().GetTransparentBackground())
	preferences := messages.Preferences{TransparentBackground: false}
	require.NoError(t, savePreferences(preferences))
	require.False(t, userconfig.Get().GetTransparentBackground())
	require.NotNil(t, userconfig.Get().TransparentBackground)
	preferences.TransparentBackground = true
	require.NoError(t, savePreferences(preferences))
	require.True(t, userconfig.Get().GetTransparentBackground())
	require.Nil(t, userconfig.Get().TransparentBackground, "default true is omitted from YAML")
}

func TestCloseTabChoicePersistenceAndSettingsMerge(t *testing.T) {
	setupSettingsConfigTest(t)
	original := preferencesFromConfig(userconfig.Get())
	assert.Equal(t, messages.CloseTabAsk, original.CloseTabChoice)
	m := newCloseTabTestModel(t)
	for _, choice := range []messages.CloseTabChoice{messages.CloseTabNo, messages.CloseTabYes} {
		require.Nil(t, m.rememberCloseTabChoice(choice))
		cfg, err := userconfig.Load()
		require.NoError(t, err)
		assert.Equal(t, choice, preferencesFromConfig(cfg.GetSettings()).CloseTabChoice)
		assert.Equal(t, choice, m.closeTabChoice)
	}
	// An older Settings draft must not erase a choice remembered after it opened.
	draft := original
	draft.Sound = true
	require.NoError(t, saveChangedPreferences(original, draft))
	assert.Equal(t, messages.CloseTabYes, preferencesFromConfig(userconfig.Get()).CloseTabChoice)
	original = preferencesFromConfig(userconfig.Get())
	draft = original
	draft.CloseTabChoice = messages.CloseTabAsk
	require.NoError(t, saveChangedPreferences(original, draft))
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.Settings.CloseTabChoice, "Ask clears persisted choice")
	assert.True(t, cfg.Settings.Sound, "changing choice preserves other preferences")
}

func TestCloseTabChoiceSaveFailureKeepsAsking(t *testing.T) {
	setupSettingsConfigTest(t)
	require.NoError(t, os.WriteFile(userconfig.Path(), []byte("settings: [broken"), 0o600))
	m := newCloseTabTestModel(t)
	require.NotNil(t, m.rememberCloseTabChoice(messages.CloseTabYes), "persistence failure produces a warning")
	assert.Equal(t, messages.CloseTabAsk, m.closeTabChoice)
}

func TestGitDiffLayoutPersistenceDefaultsAndRoundTrip(t *testing.T) {
	setupSettingsConfigTest(t)
	assert.Equal(t, messages.GitDiffUnified, messages.ParseGitDiffLayout(userconfig.Get().GetGitDiffLayout()))
	assert.NoError(t, savePreferences(messages.Preferences{GitDiffLayout: messages.GitDiffSplit}))
	assert.Equal(t, messages.GitDiffSplit, messages.ParseGitDiffLayout(userconfig.Get().GetGitDiffLayout()))
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.Equal(t, "split", cfg.GetSettings().GitDiffLayout)
	assert.NoError(t, savePreferences(messages.Preferences{GitDiffLayout: messages.GitDiffUnified}))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	assert.Empty(t, cfg.GetSettings().GitDiffLayout, "the default is omitted")
}
