package userconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScrollOnSendDefaultsAndPersistence(t *testing.T) {
	t.Parallel()
	require.False(t, (*Settings)(nil).GetScrollOnSend())
	require.False(t, (&Settings{}).GetScrollOnSend())
	for _, enabled := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, (&Config{Settings: &Settings{ScrollOnSend: enabled}}).saveTo(path))
		loaded, err := loadFrom(path, "")
		require.NoError(t, err)
		require.Equal(t, enabled, loaded.GetSettings().GetScrollOnSend())
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		if enabled {
			require.Contains(t, string(data), "scroll_on_send: true")
		} else {
			require.NotContains(t, string(data), "scroll_on_send")
		}
	}
}
