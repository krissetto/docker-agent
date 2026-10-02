package root

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestRuntimeOptsReloadsUseSubagents(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })

	flags := &runExecFlags{}
	loaded := newSessionTestLoadResult()
	runConfig := &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}
	newRuntime := func() *runtime.LocalRuntime {
		t.Helper()
		rt, err := runtime.New(t.Context(), loaded.Team, flags.runtimeOpts(loaded, runConfig, session.NewInMemorySessionStore(), "root")...)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close()) })
		return rt
	}

	assert.True(t, newRuntime().UseSubagents(), "missing preference defaults on")
	for _, enabled := range []bool{false, true, false} {
		require.NoError(t, userconfig.SetUseSubagents(enabled))
		assert.Equal(t, enabled, newRuntime().UseSubagents(), "each new or restored owner reads the latest saved preference")
	}
}

func TestSelectRemoteBackendUseSubagents(t *testing.T) {
	// Not parallel: SetConfigDir mutates process-global state.
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })

	flags := &runExecFlags{remoteAddress: "http://127.0.0.1:1"}
	b, err := flags.selectBackend("agent.yaml")
	require.NoError(t, err)
	assert.IsType(t, &remoteBackend{}, b)

	require.NoError(t, userconfig.SetUseSubagents(false))
	b, err = flags.selectBackend("agent.yaml")
	require.ErrorContains(t, err, "--remote does not support disabling subagents")
	assert.Nil(t, b)

	require.NoError(t, userconfig.SetUseSubagents(true))
	b, err = flags.selectBackend("agent.yaml")
	require.NoError(t, err)
	assert.IsType(t, &remoteBackend{}, b)
}
