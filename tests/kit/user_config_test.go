package kit_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestKitUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	configPath := filepath.Join(home, ".config", "cagent", "config.yaml")
	require.Equal(t, configPath, userconfig.Path())

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	assert.True(t, cfg.GetSettings().GetShowBanner(), "non-kit defaults remain unchanged")
	assert.False(t, cfg.GetSettings().YOLO)
	assert.Empty(t, cfg.GetSettings().GetSafety())
	assert.False(t, cfg.GetSettings().GetRestoreTabs())

	bundled, err := os.ReadFile(kitPath(t, "user-config.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o700))
	require.NoError(t, os.WriteFile(configPath, bundled, 0o600))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Settings)
	require.NotNil(t, cfg.Settings.ShowBanner)
	assert.False(t, cfg.Settings.GetShowBanner())
	assert.True(t, cfg.Settings.YOLO)
	assert.Equal(t, latest.SafetyModeAutonomous, cfg.Settings.GetSafety())
	assert.True(t, cfg.Settings.GetRestoreTabs())

	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings.ShowBanner = new(true)
		return nil
	}))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	assert.True(t, cfg.GetSettings().GetShowBanner(), "saved Settings can re-enable the banner")
	assert.True(t, cfg.GetSettings().YOLO, "unrelated saves preserve kit defaults")
	assert.True(t, cfg.GetSettings().GetRestoreTabs())

	require.NoError(t, userconfig.Update(func(cfg *userconfig.Config) error {
		cfg.Settings.YOLO = false
		cfg.Settings.RestoreTabs = new(false)
		return nil
	}))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	assert.False(t, cfg.GetSettings().YOLO, "saved Settings can disable YOLO")
	assert.Empty(t, cfg.GetSettings().GetSafety(), "no autonomous safety override remains")
	assert.False(t, cfg.GetSettings().GetRestoreTabs(), "saved Settings can disable tab restoration")

	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	assert.True(t, cfg.GetSettings().GetShowBanner(), "custom config directories do not inherit kit settings")
	assert.False(t, cfg.GetSettings().YOLO)
	assert.Empty(t, cfg.GetSettings().GetSafety())
	assert.False(t, cfg.GetSettings().GetRestoreTabs())
}
