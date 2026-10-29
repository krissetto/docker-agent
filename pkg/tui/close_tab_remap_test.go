package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// A close_tab remap must reach the tab bar, which owns its own key handling.
func TestCloseTabRemapReachesTabBar(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir(""); core.ResetKeys() })
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Keybindings: []userconfig.Keybinding{{Action: "close_tab", Keys: []string{"ctrl+q"}}}}
		return nil
	}))
	core.ResetKeys()
	assert.Equal(t, []string{"ctrl+q"}, core.GetKeys().CloseTab.Keys())
	assert.Equal(t, []string{"ctrl+q"}, tabbar.DefaultKeyMap().CloseTab.Keys())
}

// The help catalog and tab bar must advertise the remapped close-tab key.
func TestCloseTabRemapShownInHelpAndTabBar(t *testing.T) {
	setupSettingsConfigTest(t)
	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings = &userconfig.Settings{Keybindings: []userconfig.Keybinding{{Action: "close_tab", Keys: []string{"ctrl+q"}}}}
		return nil
	}))
	core.ResetKeys()
	t.Cleanup(core.ResetKeys)

	root, _, _ := wallClockRoot(t, 120, 40)
	root.focusedPanel = PanelContent
	doc := root.helpDocument()
	require.Equal(t, []string{"ctrl+q"}, helpKeys(doc.Current, "tab.close"))

	closeTab := tabbar.DefaultKeyMap().CloseTab
	assert.Equal(t, []string{"ctrl+q"}, closeTab.Keys())
	assert.Equal(t, "Ctrl+q", closeTab.Help().Key)
}
