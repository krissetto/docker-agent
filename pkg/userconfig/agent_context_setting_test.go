package userconfig

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShowAgentContextDefaultsOffAndPersists(t *testing.T) {
	t.Parallel()
	assert.False(t, (*Settings)(nil).GetShowAgentContext())
	assert.False(t, (&Settings{}).GetShowAgentContext())
	for _, enabled := range []bool{true, false} {
		configFile := filepath.Join(t.TempDir(), "config.yaml")
		config := &Config{Settings: &Settings{ShowAgentContext: enabled}}
		require.NoError(t, config.saveTo(configFile))
		loaded, err := loadFrom(configFile, "")
		require.NoError(t, err)
		assert.Equal(t, enabled, loaded.GetSettings().GetShowAgentContext())
	}
}
