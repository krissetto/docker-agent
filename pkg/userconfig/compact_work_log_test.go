package userconfig

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompactWorkLogDefaultsAndPersistence(t *testing.T) {
	t.Parallel()
	assert.True(t, (*Settings)(nil).GetCompactWorkLog())
	assert.True(t, (&Settings{}).GetCompactWorkLog())
	for _, enabled := range []bool{false, true} {
		configFile := filepath.Join(t.TempDir(), "config.yaml")
		config := &Config{Settings: &Settings{CompactWorkLog: &enabled}}
		require.NoError(t, config.saveTo(configFile))
		loaded, err := loadFrom(configFile, "")
		require.NoError(t, err)
		assert.Equal(t, enabled, loaded.GetSettings().GetCompactWorkLog())
	}
}
