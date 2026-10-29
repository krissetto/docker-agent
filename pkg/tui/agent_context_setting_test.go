package tui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestAgentContextSettingPreviewCancelSaveAndRestart(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	root.hideSidebar = false // the Sidebar layout section, which holds this row, needs a visible sidebar
	require.False(t, root.agentContextReadings, "readings are off by default")

	d := settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Agent context %")
	require.True(t, root.agentContextReadings)
	require.False(t, userconfig.Get().GetShowAgentContext(), "preview is not persisted")
	settingsCancel(t, root, d)
	require.False(t, root.agentContextReadings)

	d = settingsFromRoot(t, root)
	settingsPreview(t, root, d, "Agent context %")
	settingsSave(t, root, d)
	require.True(t, userconfig.Get().GetShowAgentContext())
	restarted, _, _ := frozenClockRoot(t, 120, 50)
	require.True(t, restarted.agentContextReadings, "a restarted TUI reads the saved preference")
}

func TestAgentContextPreferenceRoundTripsAndMergesConcurrentEdits(t *testing.T) {
	setupSettingsConfigTest(t)
	original := preferencesFromConfig(userconfig.Get())
	require.False(t, original.ShowAgentContext)

	draft := original
	draft.ShowAgentContext = true
	require.NoError(t, saveChangedPreferences(original, draft))
	require.True(t, userconfig.Get().GetShowAgentContext())

	stale := original
	stale.ShowBanner = !stale.ShowBanner
	require.NoError(t, saveChangedPreferences(original, stale))
	require.True(t, userconfig.Get().GetShowAgentContext(), "an unrelated stale draft keeps the opt-in")

	off := preferencesFromConfig(userconfig.Get())
	off.ShowAgentContext = false
	require.NoError(t, savePreferences(off))
	config, err := userconfig.Load()
	require.NoError(t, err)
	require.False(t, config.GetSettings().GetShowAgentContext(), "the default is restored on disk")
}
