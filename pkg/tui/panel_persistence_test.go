package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestPanelSettingsFromConfig(t *testing.T) {
	t.Parallel()
	assert.Equal(t, messages.DefaultPanelSettings(), panelSettingsFromConfig(nil))
	for _, elements := range [][]string{nil, {}, {"unknown", "git"}} {
		got := panelSettingsFromConfig(&userconfig.PanelSettings{Elements: elements})
		require.NotNil(t, got.Elements)
		assert.Empty(t, got.Elements)
	}
	got := panelSettingsFromConfig(&userconfig.PanelSettings{Elements: []string{"todos", "unknown", "workspace", "todos"}})
	assert.Equal(t, []messages.PanelElement{messages.PanelTodos, messages.PanelWorkspace}, got.Elements)
}

func TestPanelPreferencesPersistOrderOffAndDefaults(t *testing.T) {
	setupSettingsConfigTest(t)
	p := messages.Preferences{Layout: layoutSettingsFromConfig(userconfig.LayoutSettings{})}
	p.Panel = messages.PanelSettings{Elements: []messages.PanelElement{messages.PanelTodos, messages.PanelWorkspace, messages.PanelTodos, "unknown"}}
	require.NoError(t, savePreferences(p))
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Settings.Panel)
	assert.Equal(t, []string{"todos", "workspace"}, cfg.Settings.Panel.Elements)
	assert.True(t, panelSettingsFromConfig(cfg.Settings.GetPanel()).Equal(p.Panel))

	p.Panel = messages.PanelSettings{Elements: []messages.PanelElement{}}
	require.NoError(t, savePreferences(p))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Settings.Panel, "all-off must not be omitted as defaults")
	require.NotNil(t, panelSettingsFromConfig(cfg.Settings.GetPanel()).Elements)
	assert.Empty(t, cfg.Settings.Panel.Elements)

	p.Panel = messages.DefaultPanelSettings()
	require.NoError(t, savePreferences(p))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	assert.Nil(t, cfg.Settings.Panel, "default order is omitted")
	assert.Equal(t, messages.DefaultPanelSettings(), panelSettingsFromConfig(cfg.Settings.GetPanel()))
}
