package backgroundjobs

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestBackgroundJobsSessionOwnership(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell semantics")
	}
	definition := newTestTool(t)
	// Identical public session IDs must not confer resource ownership.
	base := httpclient.ContextWithSessionID(context.WithoutCancel(t.Context()), "same-session-id")
	first := tools.WithResourceOwner(base, tools.NewResourceOwner())
	second := tools.WithResourceOwner(base, tools.NewResourceOwner())
	t.Cleanup(func() {
		require.NoError(t, definition.StopResourceOwner(first))
		require.NoError(t, definition.StopResourceOwner(second))
	})
	all, err := definition.Tools(base)
	require.NoError(t, err)
	call := func(ctx context.Context, name, args string) *tools.ToolCallResult {
		t.Helper()
		result, err := findTool(t, all, name).Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: args}}, tools.NopRuntime{})
		require.NoError(t, err)
		return result
	}
	for _, ctx := range []context.Context{first, second} {
		result := call(ctx, ToolNameRunBackgroundJob, `{"cmd":"echo private-output; exec sleep 30"}`)
		require.False(t, result.IsError, result.Output)
	}
	var firstJob, secondJob *backgroundJob
	definition.handler.jobs.Range(func(_ string, job *backgroundJob) bool {
		if job.owner == tools.ResourceOwnerFromContext(first) {
			firstJob = job
		} else {
			secondJob = job
		}
		return true
	})
	require.NotNil(t, firstJob)
	require.NotNil(t, secondJob)
	require.Eventually(t, func() bool {
		firstJob.outputMu.RLock()
		defer firstJob.outputMu.RUnlock()
		return strings.Contains(firstJob.output.String(), "private-output")
	}, 5*time.Second, 10*time.Millisecond)

	for i, ctx := range []context.Context{first, second} {
		own, foreign := firstJob, secondJob
		if i == 1 {
			own, foreign = secondJob, firstJob
		}
		list := call(ctx, ToolNameListBackgroundJobs, `{}`)
		require.Contains(t, list.Output, own.id)
		require.NotContains(t, list.Output, foreign.id)
		view := call(ctx, ToolNameViewBackgroundJob, `{"job_id":"`+own.id+`"}`)
		require.False(t, view.IsError)
		for _, name := range []string{ToolNameViewBackgroundJob, ToolNameWaitBackgroundJob, ToolNameStopBackgroundJob} {
			denied := call(ctx, name, `{"job_id":"`+foreign.id+`","timeout":1}`)
			unknown := call(ctx, name, `{"job_id":"unknown","timeout":1}`)
			require.True(t, denied.IsError)
			require.Equal(t, strings.ReplaceAll(unknown.Output, "unknown", foreign.id), denied.Output)
			require.NotContains(t, denied.Output, "private-output")
		}
	}
	// Unscoped definition shutdown cannot terminate either session's process.
	require.NoError(t, definition.Stop(base))
	require.Equal(t, statusRunning, firstJob.status.Load())
	require.Equal(t, statusRunning, secondJob.status.Load())
	require.NoError(t, definition.Stop(first))
	require.Equal(t, statusStopped, firstJob.status.Load())
	require.Equal(t, statusRunning, secondJob.status.Load())
	wait := call(first, ToolNameWaitBackgroundJob, `{"job_id":"`+firstJob.id+`"}`)
	require.False(t, wait.IsError)
	require.Contains(t, wait.Output, "private-output")
	// Finished output remains owner-bound too.
	require.True(t, call(second, ToolNameViewBackgroundJob, `{"job_id":"`+firstJob.id+`"}`).IsError)
	stopped := call(second, ToolNameStopBackgroundJob, `{"job_id":"`+secondJob.id+`"}`)
	require.False(t, stopped.IsError, stopped.Output)
	require.Equal(t, statusStopped, secondJob.status.Load())
}

func TestBackgroundJobsRequireResourceOwner(t *testing.T) {
	t.Parallel()
	definition := newTestTool(t)
	ctx := httpclient.ContextWithSessionID(t.Context(), "not-an-owner")
	result, err := definition.handler.RunBackgroundJob(ctx, RunBackgroundJobArgs{Cmd: "echo must-not-run"}, tools.NopRuntime{})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Zero(t, definition.handler.jobs.Length())
	result, err = definition.handler.ListBackgroundJobs(ctx, nil)
	require.NoError(t, err)
	require.True(t, result.IsError)
	for _, name := range []string{ToolNameViewBackgroundJob, ToolNameWaitBackgroundJob, ToolNameStopBackgroundJob} {
		all, err := definition.Tools(ctx)
		require.NoError(t, err)
		result, err = findTool(t, all, name).Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"job_id":"anything"}`}}, tools.NopRuntime{})
		require.NoError(t, err)
		require.True(t, result.IsError)
		require.Contains(t, result.Output, "Job not found")
	}
}

func TestBackgroundJobsConcurrentOwnersAndRecall(t *testing.T) {
	t.Parallel()
	definition := newTestTool(t)
	definition.handler.recall = true
	base := context.WithoutCancel(t.Context())
	const owners = 8
	var wg sync.WaitGroup
	for range owners {
		ctx := tools.WithResourceOwner(base, tools.NewResourceOwner())
		wg.Go(func() {
			rt := &recallRuntime{recalls: make(chan string, 1)}
			defer func() { require.NoError(t, definition.StopResourceOwner(ctx)) }()
			result, err := definition.handler.RunBackgroundJobWithRecall(ctx, RunBackgroundJobRecallArgs{Cmd: "echo owner-recall", Recall: true}, rt)
			require.NoError(t, err)
			require.False(t, result.IsError, result.Output)
			// Rebinding a caller's context cannot redirect the captured job owner or recall handle.
			foreign := tools.WithResourceOwner(ctx, tools.NewResourceOwner())
			list, err := definition.handler.ListBackgroundJobs(foreign, nil)
			require.NoError(t, err)
			require.Contains(t, list.Output, "No background jobs found")
			select {
			case message := <-rt.recalls:
				list, err := definition.handler.ListBackgroundJobs(ctx, nil)
				require.NoError(t, err)
				require.Equal(t, 1, strings.Count(list.Output, "ID: job_"))
				var id string
				definition.handler.jobs.Range(func(_ string, job *backgroundJob) bool {
					if job.owner == tools.ResourceOwnerFromContext(ctx) {
						id = job.id
					}
					return true
				})
				require.NotEmpty(t, id)
				require.Contains(t, message, id)
				require.Contains(t, message, "owner-recall")
			case <-time.After(5 * time.Second):
				t.Error("timed out waiting for owner-bound recall")
			}
		})
	}
	wg.Wait()
}

func TestBackgroundJobsOwnerCleanupBoundsPipeHoldingChild(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell and process-group semantics")
	}
	definition := newTestTool(t)
	ctx := tools.WithResourceOwner(context.WithoutCancel(t.Context()), tools.NewResourceOwner())
	t.Cleanup(func() { require.NoError(t, definition.StopResourceOwner(ctx)) })
	result, err := definition.handler.RunBackgroundJob(ctx, RunBackgroundJobArgs{
		Cmd: `sleep 2 & echo child-ready; wait`,
	}, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	var job *backgroundJob
	definition.handler.jobs.Range(func(_ string, candidate *backgroundJob) bool {
		job = candidate
		return false
	})
	require.NotNil(t, job)
	require.Eventually(t, func() bool {
		job.outputMu.RLock()
		defer job.outputMu.RUnlock()
		return strings.Contains(job.output.String(), "child-ready")
	}, 5*time.Second, 10*time.Millisecond)
	started := time.Now()
	require.NoError(t, definition.StopResourceOwner(ctx))
	require.Less(t, time.Since(started), gracefulStopTimeout+forcedStopTimeout,
		"direct-process cleanup must bound the wait for child-held pipes")
	select {
	case <-job.done:
	default:
		t.Fatal("session cleanup returned before the direct shell and output monitor exited")
	}
	require.Equal(t, statusStopped, job.status.Load())
}

func TestBackgroundJobsRetirementReclaimsOnlyDrainedOwner(t *testing.T) {
	t.Parallel()
	definition := newTestTool(t)
	definition.handler.recall = true
	base := context.WithoutCancel(t.Context())
	first := tools.WithResourceOwner(base, tools.NewResourceOwner())
	second := tools.WithResourceOwner(base, tools.NewResourceOwner())
	rt := &blockedRecallRuntime{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		close(rt.release)
		require.NoError(t, definition.StopResourceOwner(first))
		require.NoError(t, definition.StopResourceOwner(second))
	})
	result, err := definition.handler.RunBackgroundJobWithRecall(first, RunBackgroundJobRecallArgs{Cmd: "echo retired-output", Recall: true}, rt)
	require.NoError(t, err)
	require.False(t, result.IsError)
	result, err = definition.handler.RunBackgroundJob(second, RunBackgroundJobArgs{Cmd: "echo surviving-output"}, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError)
	var retired, survivor *backgroundJob
	definition.handler.jobs.Range(func(_ string, job *backgroundJob) bool {
		if job.owner == tools.ResourceOwnerFromContext(first) {
			retired = job
		} else {
			survivor = job
		}
		return true
	})
	require.NotNil(t, retired)
	require.NotNil(t, survivor)
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recall monitor did not reach its completion boundary")
	}
	// Completed status alone does not mean the monitor has released its origin.
	canceled, cancel := context.WithCancel(first)
	cancel()
	require.ErrorIs(t, definition.StopResourceOwner(canceled), context.Canceled)
	_, exists := definition.handler.jobs.Load(retired.id)
	require.True(t, exists, "undrained jobs must retain cleanup authority")
	rt.release <- struct{}{}
	require.NoError(t, definition.StopResourceOwner(first))
	_, exists = definition.handler.jobs.Load(retired.id)
	require.False(t, exists)
	retired.outputMu.RLock()
	require.Nil(t, retired.rt)
	retired.outputMu.RUnlock()
	definition.handler.jobs.Range(func(_ string, job *backgroundJob) bool {
		require.NotSame(t, tools.ResourceOwnerFromContext(first), job.owner)
		return true
	})
	result, err = definition.handler.WaitBackgroundJob(second, WaitBackgroundJobArgs{JobID: survivor.id})
	require.NoError(t, err)
	require.False(t, result.IsError)
	require.Contains(t, result.Output, "surviving-output")
	require.NoError(t, definition.StopResourceOwner(second))
	require.Zero(t, definition.handler.jobs.Length())
}

type blockedRecallRuntime struct {
	tools.NopRuntime

	entered chan struct{}
	release chan struct{}
}

func (r *blockedRecallRuntime) Recall(context.Context, string) error {
	close(r.entered)
	<-r.release
	return nil
}

func (*blockedRecallRuntime) Supports(c tools.Capability) bool {
	return c == tools.CapabilityRecall
}
