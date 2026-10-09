//go:build darwin || linux

package root

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestForegroundManagedAPIAndAttach(t *testing.T) {
	home := isolateSessionsList(t)
	workspace, err := managedWorkspace(filepath.Join(home))
	require.NoError(t, err)
	base := filepath.Join(t.TempDir(), "private")
	team := filepath.Join(workspace, "team.yaml")
	require.NoError(t, os.WriteFile(team, []byte("agents:\n  root:\n    model: openai/offline-test\n    instruction: original\n  worker:\n    model: anthropic/offline-test\n"), 0600))
	dir, err := managedStateDir(base, workspace)
	require.NoError(t, err)
	start := func() (context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		f := &apiFlags{managedAPI: true, managedAPIStateDir: base, modelOverrides: []string{"openai/creation-model"}, runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}, maxRequestSize: 1 << 20}
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		go func() { result <- f.runAPICommand(cmd, []string{team}) }()
		return cancel, result
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	stop, result := start()
	t.Cleanup(stop)
	d, err := attachManagedAPI(ctx, dir)
	require.NoError(t, err)
	assert.Equal(t, os.Getpid(), d.PID, "foreground process itself owns the API")
	m, err := managedReadManifest(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"openai/creation-model"}, m.Startup().ModelOverrides)
	assert.Equal(t, workspace, m.Startup().Workspace)
	assert.Equal(t, team, m.Startup().SourceKey)
	entries, err := filepath.Glob(filepath.Join(dir, "candidate-*"))
	require.NoError(t, err)
	assert.Empty(t, entries, "foreground startup has no manifest rendezvous")
	f := &runExecFlags{managedAPI: true, managedAPIAttach: true, managedAPIStateDir: base, runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}}
	source, err := f.bootstrapManagedAPI(ctx, "not-a-real-source")
	require.NoError(t, err, "attach uses committed source, never resolves a client team")
	assert.Equal(t, team, source)
	assert.Equal(t, d.Address, f.remoteAddress)
	ids, err := attachedManagedSessionIDs(ctx, base, workspace)
	require.NoError(t, err)
	assert.Empty(t, ids, "catalog and readiness do not create sessions or providers")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, d.Address+"/api/v2/server", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	_ = response.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
	list := newSessionsListCmd()
	var listed bytes.Buffer
	list.SetOut(&listed)
	list.SetArgs([]string{"--managed-api", "--managed-api-attach", "--working-dir", workspace, "--managed-api-state-dir", base, "--quiet"})
	require.NoError(t, list.ExecuteContext(ctx))
	assert.Empty(t, listed.String())
	token, err := managedRead(filepath.Join(dir, "token"))
	require.NoError(t, err)
	second := &apiFlags{managedState: dir, manifest: m}
	_, err = second.prepareManagedDaemon()
	require.ErrorContains(t, err, "already has an owner")
	require.NoError(t, managedProbe(ctx, dir, d, m))
	stop()
	require.NoError(t, <-result)
	absentCtx, absentCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer absentCancel()
	_, err = attachManagedAPI(absentCtx, dir)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "no daemon was started")
	stop2, result2 := start()
	t.Cleanup(stop2)
	restarted, err := attachManagedAPI(ctx, dir)
	require.NoError(t, err)
	assert.Equal(t, d.Address, restarted.Address)
	assert.NotEqual(t, d.InstanceID, restarted.InstanceID)
	nextToken, err := managedRead(filepath.Join(dir, "token"))
	require.NoError(t, err)
	assert.Equal(t, token, nextToken)
	stop2()
	require.NoError(t, <-result2)
	require.NoError(t, os.WriteFile(team, []byte("agents:\n  root:\n    model: openai/changed\n"), 0600))
	changed := &apiFlags{managedAPI: true, managedAPIStateDir: base, modelOverrides: []string{"openai/creation-model"}, runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}}
	require.ErrorContains(t, changed.prepareForegroundManagedAPI(ctx, team), "configuration differs")
	saved, err := managedReadManifest(dir)
	require.NoError(t, err)
	require.NoError(t, managedCompatible(saved, m))
}

func TestManagedAttachMissingHookNeverStarts(t *testing.T) {
	dir, _ := managedFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Millisecond)
	defer cancel()
	_, err := attachManagedAPI(ctx, dir)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "restart the sandbox")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "attach creates no token, locks, manifest or database")
}

func TestManagedAttachCLIRejectsServerChanges(t *testing.T) {
	home := isolateSessionsList(t)
	for _, args := range [][]string{
		{"--model", "openai/changed"}, {"--working-dir", home}, {"--flavor", "changed"},
		{"--env-from-file", "changed"}, {"--models-gateway", "http://changed"}, {"--hook-stop", "true"},
	} {
		cmd := newRunCmd()
		require.NoError(t, cmd.ParseFlags(args))
		err := validateManagedAttachFlags(cmd)
		require.ErrorContains(t, err, "bind server configuration")
	}
	cmd := newRunCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--exec", "--session=-1", "--safety", "strict", "--yolo=false", "--dry-run"}))
	require.NoError(t, validateManagedAttachFlags(cmd))
	var output bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"run", "default", "--managed-api-attach"})
	require.ErrorContains(t, root.ExecuteContext(t.Context()), "requires --managed-api")
	root = NewRootCmd()
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"sessions", "list", "--managed-api-attach"})
	require.ErrorContains(t, root.ExecuteContext(t.Context()), "requires --managed-api")
	t.Setenv("ASYNC_AGENT_KIT_CONNECTION", "1")
	root = NewRootCmd()
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"run", "default", "--managed-api=false", "--managed-api-attach=false", "--dry-run"})
	require.ErrorContains(t, root.ExecuteContext(t.Context()), "managed mode cannot be disabled")
	_, err := os.Stat(filepath.Join(home, ".cagent", "session.db"))
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestForegroundManagedAPIPrivateState(t *testing.T) {
	home := isolateSessionsList(t)
	workspace := filepath.Join(home, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0700))
	f := &apiFlags{managedAPI: true, managedAPIStateDir: filepath.Join(workspace, "state"), runConfig: config.RuntimeConfig{Config: config.Config{WorkingDir: workspace}}}
	require.ErrorContains(t, f.prepareForegroundManagedAPI(t.Context(), "default"), "outside the mounted workspace")
	assert.NoDirExists(t, filepath.Join(workspace, "state"))
	files, err := filepath.Glob(filepath.Join(workspace, "state", "*", "token"))
	require.NoError(t, err)
	assert.Empty(t, files)
}
