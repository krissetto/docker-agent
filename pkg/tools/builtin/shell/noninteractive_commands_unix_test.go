//go:build !windows && !js

package shell_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/backgroundjobs"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
)

func TestAgentCommandsDetachControllingTerminal(t *testing.T) {
	if os.Getenv("DOCKER_AGENT_TEST_WITH_CONTROLLING_TTY") == "1" {
		checkAgentCommandsWithoutTerminal(t)
		return
	}
	t.Parallel()

	master, terminal, err := pty.Open()
	require.NoError(t, err)
	defer master.Close()
	defer terminal.Close()

	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestAgentCommandsDetachControllingTerminal$", "-test.v")
	cmd.Env = append(os.Environ(), "DOCKER_AGENT_TEST_WITH_CONTROLLING_TTY=1", "SHELL=/bin/sh")
	cmd.Stdin = terminal
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func checkAgentCommandsWithoutTerminal(t *testing.T) {
	t.Helper()
	// The parent has a controlling terminal even though its stdout/stderr are captured.
	terminal, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, terminal.Close())

	const probe = `if (: > /dev/tty) 2>/dev/null; then echo inherited-terminal; exit 1; fi; echo detached-terminal; echo captured-stderr >&2`
	cfg := &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}

	t.Run("shell", func(t *testing.T) {
		toolset := shell.New(nil, cfg)
		all, err := toolset.Tools(t.Context())
		require.NoError(t, err)
		result, err := all[0].Handler(t.Context(), tools.ToolCall{
			Function: tools.FunctionCall{Arguments: `{"cmd":"` + probe + `"}`},
		}, tools.NopRuntime{})
		require.NoError(t, err)
		assert.False(t, result.IsError, result.Output)
		assert.Contains(t, result.Output, "detached-terminal")
		assert.Contains(t, result.Output, "captured-stderr")
		assert.NotContains(t, result.Output, "inherited-terminal")
	})

	t.Run("script", func(t *testing.T) {
		toolset, err := shell.NewScript(map[string]latest.ScriptShellToolConfig{
			"probe": {Cmd: probe},
		}, nil)
		require.NoError(t, err)
		all, err := toolset.Tools(t.Context())
		require.NoError(t, err)
		result, err := all[0].Handler(t.Context(), tools.ToolCall{
			Function: tools.FunctionCall{Arguments: `{}`},
		}, tools.NopRuntime{})
		require.NoError(t, err)
		assert.False(t, result.IsError, result.Output)
		assert.Contains(t, result.Output, "detached-terminal")
		assert.Contains(t, result.Output, "captured-stderr")
		assert.NotContains(t, result.Output, "inherited-terminal")
	})

	t.Run("background job", func(t *testing.T) {
		ctx := tools.WithResourceOwner(t.Context(), tools.NewResourceOwner())
		toolset := backgroundjobs.New(nil, cfg)
		require.NoError(t, toolset.Start(ctx))
		t.Cleanup(func() { _ = toolset.Stop(ctx) })
		all, err := toolset.Tools(ctx)
		require.NoError(t, err)
		var run, wait tools.Tool
		for _, tool := range all {
			switch tool.Name {
			case backgroundjobs.ToolNameRunBackgroundJob:
				run = tool
			case backgroundjobs.ToolNameWaitBackgroundJob:
				wait = tool
			}
		}
		result, err := run.Handler(ctx, tools.ToolCall{
			Function: tools.FunctionCall{Arguments: `{"cmd":"` + probe + `"}`},
		}, tools.NopRuntime{})
		require.NoError(t, err)
		require.False(t, result.IsError, result.Output)
		const prefix = "Background job started with ID: "
		jobID, _, _ := strings.Cut(strings.TrimPrefix(result.Output, prefix), "\n")
		require.NotEmpty(t, jobID)
		result, err = wait.Handler(ctx, tools.ToolCall{
			Function: tools.FunctionCall{Arguments: `{"job_id":"` + jobID + `"}`},
		}, tools.NopRuntime{})
		require.NoError(t, err)
		assert.Contains(t, result.Output, "detached-terminal")
		assert.Contains(t, result.Output, "captured-stderr")
		assert.Contains(t, result.Output, "Exit Code: 0")
	})

	t.Run("credential helper", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fake-op")
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+probe+"\n"), 0o700))
		value, found := environment.NewCredentialHelperProvider(path).Get(t.Context(), environment.DockerDesktopTokenEnv)
		assert.True(t, found)
		assert.Equal(t, "detached-terminal", value)
	})
}
