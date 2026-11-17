package tui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/userconfig"
)

type scrollSettingPage struct {
	chat.Page
	enabled bool
}

func (p *scrollSettingPage) SetScrollOnSend(enabled bool) { p.enabled = enabled }

func TestScrollOnSendPreviewCancelSaveAndRestart(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	require.False(t, root.scrollOnSend)
	page := &scrollSettingPage{Page: root.chatPage}
	root.chatPage, root.chatPages[root.paneFocus()] = page, page
	d := settingsFromRoot(t, root)
	settingsClickRendered(t, d, "Behavior")
	settingsPreview(t, root, d, "Scroll new prompt to top")
	require.True(t, root.scrollOnSend)
	require.True(t, page.enabled)
	require.False(t, userconfig.Get().GetScrollOnSend())
	settingsCancel(t, root, d)
	require.False(t, root.scrollOnSend)
	require.False(t, page.enabled)
	d = settingsFromRoot(t, root)
	settingsClickRendered(t, d, "Behavior")
	settingsPreview(t, root, d, "Scroll new prompt to top")
	settingsSave(t, root, d)
	require.True(t, userconfig.Get().GetScrollOnSend())
	restarted, _, _ := frozenClockRoot(t, 120, 50)
	require.True(t, restarted.scrollOnSend)
}

func TestScrollOnSendPreferencesConcurrentMergeAndOmittedDefault(t *testing.T) {
	setupSettingsConfigTest(t)
	original := preferencesFromConfig(userconfig.Get())
	require.False(t, original.ScrollOnSend)
	draft := original
	draft.ShowBanner = !draft.ShowBanner
	require.NoError(t, userconfig.Update(func(c *userconfig.Config) error {
		c.Settings = &userconfig.Settings{ScrollOnSend: true}
		return nil
	}))
	require.NoError(t, saveChangedPreferences(original, draft))
	require.True(t, userconfig.Get().GetScrollOnSend())
	current := preferencesFromConfig(userconfig.Get())
	current.ScrollOnSend = false
	require.NoError(t, savePreferences(current))
	loaded, err := userconfig.Load()
	require.NoError(t, err)
	require.False(t, loaded.GetSettings().GetScrollOnSend())
}
