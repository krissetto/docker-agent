package teststate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
)

func TestRunIsolatesDefaultsWithoutReplacingFixturesOrToolCaches(t *testing.T) {
	caller := t.TempDir()
	variables := []string{
		"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
		"XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "APPDATA", "LOCALAPPDATA", "DOCKER_CONFIG",
		"DOCKER_CERT_PATH", "DOCKER_AGENT_CONFIG_DIR", "CAGENT_CONFIG_DIR",
	}
	for _, name := range variables {
		t.Setenv(name, caller)
	}
	paths.SetRoot(caller)
	t.Cleanup(func() { paths.SetRoot("") })
	cacheEnv := make(map[string]string)
	for _, name := range []string{"GOPATH", "GOMODCACHE", "GOCACHE", "GOTMPDIR", "GOTOOLCHAIN", "GOFLAGS", "TMPDIR"} {
		cacheEnv[name] = os.Getenv(name)
	}
	var private string
	code := Run(func() int {
		private = os.Getenv("HOME")
		require.NotEqual(t, caller, private)
		for _, name := range variables {
			relative, err := filepath.Rel(private, os.Getenv(name))
			require.NoError(t, err)
			require.True(t, filepath.IsLocal(relative), "%s must use private state", name)
			require.DirExists(t, os.Getenv(name))
		}
		require.Equal(t, filepath.Join(private, ".cagent"), paths.GetDataDir())
		require.Equal(t, filepath.Join(private, ".config", "cagent"), paths.GetConfigDir())
		fixture := t.TempDir()
		paths.SetRoot(fixture)
		require.Equal(t, filepath.Join(fixture, "data"), paths.GetDataDir())
		paths.SetRoot("")
		require.Equal(t, filepath.Join(private, ".cagent"), paths.GetDataDir())
		return 23
	})
	require.Equal(t, 23, code)
	require.NoDirExists(t, private)
	for name, value := range cacheEnv {
		require.Equal(t, value, os.Getenv(name), "%s must be preserved", name)
	}
}
