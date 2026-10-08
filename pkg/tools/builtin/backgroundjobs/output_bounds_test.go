package backgroundjobs

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackgroundJobRecallBoundsLogAndPreservesStatus(t *testing.T) {
	t.Parallel()
	job := &backgroundJob{id: "job_1", cmd: strings.Repeat("c", 100_000)}
	got := formatBackgroundJobRecall(job, statusFailed, 42, strings.Repeat("世", 4*1024*1024)+"last diagnostic")
	assert.LessOrEqual(t, len(got), 50*1024)
	assert.Contains(t, got, "job_1 finished with status failed (exit code 42)")
	assert.Contains(t, got, "last diagnostic")
	assert.Contains(t, got, "view_background_job")
	assert.Equal(t, got, strings.ToValidUTF8(got, ""))
}

func TestBackgroundJobViewAndWaitOutputPipeline(t *testing.T) {
	t.Parallel()
	tool := newTestTool(t)
	payload := strings.Repeat("x", maxBackgroundJobOutputBytes)
	ctx := tools.WithResourceOwner(t.Context(), tools.NewResourceOwner())
	job := &backgroundJob{owner: tools.ResourceOwnerFromContext(ctx), id: "job_big", cmd: "download", output: bytes.NewBufferString(payload), startTime: time.Now(), done: make(chan struct{}), exitCode: 7}
	job.status.Store(statusFailed)
	close(job.done)
	tool.handler.jobs.Store(job.id, job)
	registry := hooks.NewRegistry()
	require.NoError(t, builtins.Register(registry))
	executor := hooks.NewExecutorWithRegistry(builtins.ApplyAgentDefaults(nil, builtins.AgentDefaults{}), t.TempDir(), nil, registry)
	t.Cleanup(func() {
		_, _ = executor.Dispatch(context.WithoutCancel(ctx), hooks.EventSessionEnd, &hooks.Input{SessionID: t.Name()})
	})
	for _, name := range []string{ToolNameViewBackgroundJob, ToolNameWaitBackgroundJob} {
		var result *tools.ToolCallResult
		var err error
		if name == ToolNameViewBackgroundJob {
			result, err = tool.handler.ViewBackgroundJob(ctx, ViewBackgroundJobArgs{JobID: job.id})
		} else {
			result, err = tool.handler.WaitBackgroundJob(ctx, WaitBackgroundJobArgs{JobID: job.id})
		}
		require.NoError(t, err)
		transformed, err := executor.Dispatch(ctx, hooks.EventToolResponseTransform, &hooks.Input{SessionID: t.Name(), ToolCategory: "background_jobs", ToolName: name, ToolResponse: result.Output})
		require.NoError(t, err)
		require.NotNil(t, transformed.UpdatedToolResponse)
		got := *transformed.UpdatedToolResponse
		assert.LessOrEqual(t, len(got), 50*1024)
		assert.Contains(t, got, "Job ID: job_big")
		assert.Contains(t, got, "Status: failed")
		assert.Contains(t, got, "Exit Code: 7")
		assert.Contains(t, got, "available in a file:")
	}
	assert.Equal(t, payload, job.output.String())
}
