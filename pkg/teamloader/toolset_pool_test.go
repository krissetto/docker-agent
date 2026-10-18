package teamloader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/backgroundjobs"
	"github.com/goccy/go-yaml"
)

func acquireBackgroundJobs(t *testing.T, pool *ToolsetLeasePool, scope, workingDir, marker string, created *atomic.Int32) (tools.ToolSet, *toolsetPoolLease) {
	t.Helper()
	toolset := latest.Toolset{Type: "background_jobs", Env: map[string]string{"POOL_MARKER": marker}}
	runConfig := &config.RuntimeConfig{Config: config.Config{WorkingDir: workingDir}}
	env, err := backgroundjobs.ResolveEnvironment(t.Context(), toolset, runConfig)
	require.NoError(t, err)
	encoded, err := yaml.Marshal(toolset)
	require.NoError(t, err)
	material, err := makeToolsetPoolKey("team.yaml", workingDir, scope, workingDir, string(encoded), env)
	require.NoError(t, err)
	createdFn := func() (tools.ToolSet, error) {
		if created != nil {
			created.Add(1)
		}
		return backgroundjobs.CreateToolSetWithEnvironment(toolset, runConfig, env), nil
	}
	dispose := func(ctx context.Context, ts tools.ToolSet) error {
		owner, ok := tools.As[*backgroundjobs.ToolSet](ts)
		if !ok {
			return errors.New("unexpected background_jobs raw owner")
		}
		return owner.CloseAll(ctx)
	}
	leaseTool, closer, err := pool.acquire(t.Context(), material, createdFn, dispose)
	require.NoError(t, err)
	lease, ok := closer.(*toolsetPoolLease)
	require.True(t, ok)
	return leaseTool, lease
}

func callPooledBackgroundJob(t *testing.T, toolset tools.ToolSet, owner *tools.ResourceOwner, name, args string) *tools.ToolCallResult {
	t.Helper()
	list, err := toolset.Tools(t.Context())
	require.NoError(t, err)
	for _, tool := range list {
		if tool.Name == name {
			result, err := tool.Handler(tools.WithResourceOwner(t.Context(), owner), tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: args}}, tools.NopRuntime{})
			require.NoError(t, err)
			return result
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func TestBackgroundJobProcessSurvivesTeamRevisionWhileResourceOwnerIsPinned(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell process continuity test uses POSIX shell")
	}
	pool, err := NewToolsetLeasePool("source-and-runtime")
	require.NoError(t, err)
	var created atomic.Int32
	wd := t.TempDir()
	file := filepath.Join(wd, "still-running")
	oldTool, oldLease := acquireBackgroundJobs(t, pool, "team-source-v1", wd, "one", &created)
	// Runtime builders provide the new lease before retiring the old team.
	newTool, newLease := acquireBackgroundJobs(t, pool, "team-source-v1", wd, "one", &created)
	rawOwner, ok := tools.As[*backgroundjobs.ToolSet](oldTool)
	require.True(t, ok)
	newRaw, ok := tools.As[*backgroundjobs.ToolSet](newTool)
	require.True(t, ok)
	assert.Same(t, rawOwner, newRaw)
	require.EqualValues(t, 1, created.Load())
	oldStart, ok := tools.As[tools.Startable](oldTool)
	require.True(t, ok)
	require.NoError(t, oldStart.Start(t.Context()))
	newStart, ok := tools.As[tools.Startable](newTool)
	require.True(t, ok)
	require.NoError(t, newStart.Start(t.Context()))
	owner := tools.NewResourceOwner()
	command := "touch " + file + "; exec sleep 30"
	started := callPooledBackgroundJob(t, oldTool, owner, backgroundjobs.ToolNameRunBackgroundJob, mustJSON(t, map[string]any{"cmd": command}))
	require.False(t, started.IsError, started.Output)
	require.Eventually(t, func() bool { _, err := os.Stat(file); return err == nil }, time.Second, 5*time.Millisecond)
	// Retirement drops only the first team lease; the same user/session's
	// running process and owner map remain usable through the current team.
	require.NoError(t, oldLease.Close())
	listed := callPooledBackgroundJob(t, newTool, owner, backgroundjobs.ToolNameListBackgroundJobs, "{}")
	assert.Contains(t, listed.Output, "running")
	otherOwnerList := callPooledBackgroundJob(t, newTool, tools.NewResourceOwner(), backgroundjobs.ToolNameListBackgroundJobs, "{}")
	assert.Contains(t, otherOwnerList.Output, "No background jobs")
	assert.FileExists(t, file, "the process survives team revision retirement while session lease remains")
	ownerCleanup := tools.WithResourceOwner(t.Context(), owner)
	require.NoError(t, tools.StopResourceOwners(ownerCleanup, newTool))
	require.Zero(t, rawOwner.JobCount(), "session-owner cleanup is isolated from raw pool retirement")
	require.NoError(t, newLease.Close())
	require.NoError(t, pool.Close(t.Context()), "final runtime pool shutdown closes raw resource owner")
}

func TestBackgroundJobsEffectiveConfigOrEnvironmentNeverReuseSameOwner(t *testing.T) {
	t.Parallel()
	pool, err := NewToolsetLeasePool("one-source-runtime")
	require.NoError(t, err)
	var created atomic.Int32
	wd := t.TempDir()
	first, firstLease := acquireBackgroundJobs(t, pool, "scope", wd, "alpha", &created)
	second, secondLease := acquireBackgroundJobs(t, pool, "scope", wd, "beta", &created)
	firstRaw, ok := tools.As[*backgroundjobs.ToolSet](first)
	require.True(t, ok)
	secondRaw, ok := tools.As[*backgroundjobs.ToolSet](second)
	require.True(t, ok)
	assert.NotSame(t, firstRaw, secondRaw)
	require.EqualValues(t, 2, created.Load())
	require.NoError(t, firstLease.Close())
	require.NoError(t, secondLease.Close())
	require.NoError(t, pool.Close(t.Context()))
}

func TestToolsetPoolFailedCreateAndStartDoNotPoisonNextAcquisition(t *testing.T) {
	t.Parallel()
	pool, err := NewToolsetLeasePool("scope")
	require.NoError(t, err)
	material := []byte("same-key")
	var attempts atomic.Int32
	_, _, err = pool.acquire(t.Context(), material, func() (tools.ToolSet, error) { attempts.Add(1); return nil, errors.New("partial create") }, func(context.Context, tools.ToolSet) error { return nil })
	require.ErrorContains(t, err, "partial create")
	tool, lease := acquireBackgroundJobs(t, pool, "scope", t.TempDir(), "stable", nil)
	start, ok := tools.As[tools.Startable](tool)
	require.True(t, ok)
	require.NoError(t, start.Start(t.Context()))
	require.NoError(t, lease.Close())
	require.NoError(t, pool.Close(t.Context()))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

func TestSharedBackgroundJobToolsetRetainsDistinctResourceOwnerIsolation(t *testing.T) {
	t.Parallel()
	pool, err := NewToolsetLeasePool("one-source-runtime")
	require.NoError(t, err)
	var created atomic.Int32
	wd := t.TempDir()
	first, lease1 := acquireBackgroundJobs(t, pool, "team-source", wd, "same", &created)
	second, lease2 := acquireBackgroundJobs(t, pool, "team-source", wd, "same", &created)
	firstRaw, ok := tools.As[*backgroundjobs.ToolSet](first)
	require.True(t, ok)
	secondRaw, ok := tools.As[*backgroundjobs.ToolSet](second)
	require.True(t, ok)
	assert.Same(t, firstRaw, secondRaw)
	owner1, owner2 := tools.NewResourceOwner(), tools.NewResourceOwner()
	callPooledBackgroundJob(t, first, owner1, backgroundjobs.ToolNameRunBackgroundJob, mustJSON(t, map[string]any{"cmd": "echo one"}))
	listed := callPooledBackgroundJob(t, second, owner2, backgroundjobs.ToolNameListBackgroundJobs, "{}")
	assert.Contains(t, listed.Output, "No background jobs")
	callPooledBackgroundJob(t, second, owner2, backgroundjobs.ToolNameRunBackgroundJob, mustJSON(t, map[string]any{"cmd": "echo two"}))
	list1 := callPooledBackgroundJob(t, second, owner1, backgroundjobs.ToolNameListBackgroundJobs, "{}")
	list2 := callPooledBackgroundJob(t, first, owner2, backgroundjobs.ToolNameListBackgroundJobs, "{}")
	assert.Contains(t, list1.Output, "one")
	assert.NotContains(t, list1.Output, "two")
	assert.Contains(t, list2.Output, "two")
	assert.NotContains(t, list2.Output, "one")
	require.NoError(t, tools.StopResourceOwners(tools.WithResourceOwner(t.Context(), owner1), first))
	require.NoError(t, tools.StopResourceOwners(tools.WithResourceOwner(t.Context(), owner2), second))
	require.NoError(t, lease1.Close())
	require.NoError(t, lease2.Close())
	require.NoError(t, pool.Close(t.Context()))
}

type poolProbeToolset struct {
	starts atomic.Int32
	closes atomic.Int32
}

func (p *poolProbeToolset) Start(context.Context) error                 { p.starts.Add(1); return nil }
func (p *poolProbeToolset) Stop(context.Context) error                  { return nil }
func (p *poolProbeToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (p *poolProbeToolset) Close() error                                { p.closes.Add(1); return nil }

type flakyStartPoolToolset struct {
	poolProbeToolset
	fail atomic.Bool
}

func (p *flakyStartPoolToolset) Start(context.Context) error {
	p.starts.Add(1)
	if p.fail.Swap(false) {
		return errors.New("injected start failure")
	}
	return nil
}

func TestToolsetLeasePoolFailedRawStartCanBeRetriedByOtherLease(t *testing.T) {
	t.Parallel()
	pool, err := NewToolsetLeasePool("source-runtime")
	require.NoError(t, err)
	raw := new(flakyStartPoolToolset)
	raw.fail.Store(true)
	create := func() (tools.ToolSet, error) { return raw, nil }
	dispose := func(context.Context, tools.ToolSet) error { return nil }
	first, firstClose, err := pool.acquire(t.Context(), []byte("key"), create, dispose)
	require.NoError(t, err)
	startable, ok := tools.As[tools.Startable](first)
	require.True(t, ok)
	require.ErrorContains(t, startable.Start(t.Context()), "injected start failure")
	second, secondClose, err := pool.acquire(t.Context(), []byte("key"), create, dispose)
	require.NoError(t, err)
	secondStart, ok := tools.As[tools.Startable](second)
	require.True(t, ok)
	require.NoError(t, secondStart.Start(t.Context()))
	require.NoError(t, firstClose.Close())
	require.NoError(t, secondClose.Close())
	require.NoError(t, pool.Close(t.Context()))
	require.EqualValues(t, 2, raw.starts.Load())
}
