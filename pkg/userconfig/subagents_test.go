package userconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

func TestSettings_GetUseSubagents(t *testing.T) {
	t.Parallel()

	assert.True(t, (*Settings)(nil).GetUseSubagents())
	assert.True(t, (&Settings{}).GetUseSubagents())
	assert.True(t, (&Settings{UseSubagents: new(true)}).GetUseSubagents())
	assert.False(t, (&Settings{UseSubagents: new(false)}).GetUseSubagents())
}

func TestSettings_UseSubagentsRoundTrip(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		content string
		want    bool
	}{
		{"missing file", "", true},
		{"missing settings", "version: v1\n", true},
		{"missing preference", "settings:\n  theme: dark\n", true},
		{"null preference", "settings:\n  use_subagents: null\n", true},
		{"disabled", "settings:\n  use_subagents: false\n", false},
		{"enabled", "settings:\n  use_subagents: true\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.yaml")
			if tt.content != "" {
				require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o600))
			}
			cfg, err := loadFrom(path, "")
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.GetSettings().GetUseSubagents())

			require.NoError(t, cfg.saveTo(path))
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			if !tt.want {
				assert.Contains(t, string(data), "use_subagents: false")
			}
			reloaded, err := loadFrom(path, "")
			require.NoError(t, err)
			assert.Equal(t, tt.want, reloaded.GetSettings().GetUseSubagents())
		})
	}
}

func TestSetUseSubagentsPreservesFreshConfig(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })

	assert.True(t, Get().GetUseSubagents())
	require.NoError(t, SetUseSubagents(false))
	assert.False(t, Get().GetUseSubagents())

	// Simulate another settings writer after the preference was loaded.
	require.NoError(t, Update(func(cfg *Config) error {
		cfg.Settings.Theme = "dark"
		cfg.Settings.Extra = map[string]any{"future_flag": "keep"}
		cfg.ModelsGateway = "https://gateway.example.com"
		return cfg.SetAlias("dev", &Alias{Path: "./dev.yaml"})
	}))
	assert.False(t, Get().GetUseSubagents(), "unrelated updates preserve explicit false")

	for _, enabled := range []bool{true, false} {
		require.NoError(t, SetUseSubagents(enabled))
		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, enabled, cfg.GetSettings().GetUseSubagents())
		assert.Equal(t, "dark", cfg.Settings.Theme)
		assert.Equal(t, "keep", cfg.Settings.Extra["future_flag"])
		assert.Equal(t, "https://gateway.example.com", cfg.ModelsGateway)
		assert.Equal(t, "./dev.yaml", cfg.Aliases["dev"].Path)
	}
}
