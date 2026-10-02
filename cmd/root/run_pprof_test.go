package root

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/cli"
)

func TestRunPprofFlag(t *testing.T) {
	t.Parallel()
	cmd := newRunCmd()
	flag := cmd.PersistentFlags().Lookup("pprof-addr")
	require.NotNil(t, flag)
	assert.True(t, flag.Hidden)
	assert.Empty(t, flag.DefValue)
	require.NoError(t, cmd.ParseFlags([]string{"--pprof-addr", "127.0.0.1:6060"}))
	assert.Equal(t, "127.0.0.1:6060", flag.Value.String())
}

func TestRunPprofResolution(t *testing.T) {
	flagListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer flagListener.Close()
	envListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer envListener.Close()
	flagAddr, envAddr := flagListener.Addr().String(), envListener.Addr().String()

	for _, tt := range []struct {
		name, flag, env, want string
	}{
		{name: "disabled", want: "failed to create CPU profile"},
		{name: "environment", env: envAddr, want: "pprof: listen on " + envAddr},
		{name: "flag", flag: flagAddr, want: "pprof: listen on " + flagAddr},
		{name: "flag wins", flag: flagAddr, env: envAddr, want: "pprof: listen on " + flagAddr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CAGENT_PPROF_ADDR", tt.env)
			// Stop before any config, daemon, provider or TUI initialization.
			f := runExecFlags{pprofAddr: tt.flag, cpuProfile: filepath.Join(t.TempDir(), "missing", "cpu.pprof")}
			err := f.runOrExec(t.Context(), cli.NewPrinter(io.Discard), nil, false)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestRunPprofStopsOnStartupFailure(t *testing.T) {
	t.Setenv("CAGENT_PPROF_ADDR", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f := runExecFlags{pprofAddr: addr, cpuProfile: filepath.Join(t.TempDir(), "missing", "cpu.pprof")}
	err = f.runOrExec(ctx, cli.NewPrinter(io.Discard), nil, false)
	require.ErrorContains(t, err, "failed to create CPU profile")
	require.NoError(t, ctx.Err(), "the command's parent context remains alive")
	require.Eventually(t, func() bool {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		ln.Close()
		return true
	}, 3*time.Second, 10*time.Millisecond, "run failure must release the pprof listener")
}

func TestManagedAPIEnvExcludesClientPprof(t *testing.T) {
	t.Setenv("CAGENT_PPROF_ADDR", "127.0.0.1:6060")
	t.Setenv("DOCKER_AGENT_TEST_ENV_PRESERVED", "yes")
	env := managedAPIEnv()
	for _, entry := range env {
		assert.False(t, strings.HasPrefix(entry, "CAGENT_PPROF_ADDR="))
	}
	assert.Contains(t, env, "DOCKER_AGENT_TEST_ENV_PRESERVED=yes")
	assert.Equal(t, "127.0.0.1:6060", os.Getenv("CAGENT_PPROF_ADDR"), "must not mutate the client environment")
}
