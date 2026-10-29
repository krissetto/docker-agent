package shellprocess

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func newTestTool(t *testing.T) *Manager {
	t.Helper()
	return New()
}

func TestRunningProcessToolsAreNarrowDiagnostics(t *testing.T) {
	tool := newTestTool(t)
	all, err := tool.ShellInspectionTools()
	require.NoError(t, err)
	require.Len(t, all, 3)
	got := make([]string, 0, len(all))
	for _, candidate := range all {
		got = append(got, candidate.Name)
		require.NotNil(t, candidate.OutputSchema)
		if candidate.Name == ToolNameInspectRunningProcess || candidate.Name == ToolNameKillRunningProcess {
			schema, err := tools.SchemaToMap(candidate.Parameters)
			require.NoError(t, err)
			assert.Equal(t, "object", schema["type"])
			props := schema["properties"].(map[string]any)
			assert.Contains(t, props, "process_id")
			assert.NotContains(t, props, "cmd")
			assert.NotContains(t, props, "timeout")
		}
	}
	assert.Equal(t, []string{ToolNameListRunningProcesses, ToolNameInspectRunningProcess, ToolNameKillRunningProcess}, got)
	for _, candidate := range all {
		if candidate.Name == ToolNameListRunningProcesses || candidate.Name == ToolNameInspectRunningProcess {
			assert.Contains(t, candidate.Description, "Completion arrives automatically")
			assert.Contains(t, candidate.Description, "wakes/resumes this agent")
			assert.Contains(t, candidate.Description, "never poll")
			assert.Contains(t, candidate.Description, "concrete diagnostic or user request")
		}
		if candidate.Name == ToolNameKillRunningProcess {
			assert.Contains(t, candidate.Description, "Intentionally cancel")
			assert.Contains(t, candidate.Description, "never kill just to end your turn")
		}
	}
	processIDSchema, err := tools.SchemaToMap(all[1].Parameters)
	require.NoError(t, err)
	processID := processIDSchema["properties"].(map[string]any)["process_id"].(map[string]any)
	assert.Contains(t, processID["description"], "shell tool when a command continues")
	assert.Contains(t, processID["description"], "one-off")
	assert.Contains(t, processID["description"], "not for polling")
}

func TestBackgroundCompletionOnlyAdvertisesAutomaticDelivery(t *testing.T) {
	message := formatShellJobCompletion(&shellProcess{id: "job", cmd: "build"}, statusCompleted, 0, "done")
	assert.Contains(t, message, "Completion output is delivered automatically and is not retained after delivery.")
	assert.Contains(t, message, "Shell command job with status completed")
	assert.NotContains(t, message, "Never poll")
	assert.NotContains(t, message, "end your turn")
}

func TestProcessManagerIsNotAModelToolset(t *testing.T) {
	_, ok := any(New()).(tools.ToolSet)
	require.False(t, ok)
}

func TestRunningProcessDiagnosticsAreSessionOwnedAndRunningOnly(t *testing.T) {
	tool := newTestTool(t)
	firstOwner, secondOwner := tools.NewResourceOwner(), tools.NewResourceOwner()
	first := &shellProcess{owner: firstOwner, id: "proc_first", cmd: "sleep 30", output: bytes.NewBufferString("private"), startTime: time.Now(), done: make(chan struct{}), shellOwned: true}
	first.status.Store(statusRunning)
	second := &shellProcess{owner: secondOwner, id: "proc_second", cmd: "echo other", output: &bytes.Buffer{}, startTime: time.Now(), done: make(chan struct{}), shellOwned: true}
	second.status.Store(statusRunning)
	completed := &shellProcess{owner: firstOwner, id: "proc_done", cmd: "echo done", output: &bytes.Buffer{}, startTime: time.Now(), done: make(chan struct{}), shellOwned: true}
	completed.status.Store(statusCompleted)
	manual := &shellProcess{owner: firstOwner, id: "proc_manual", cmd: "echo manual", output: &bytes.Buffer{}, startTime: time.Now(), done: make(chan struct{})}
	manual.status.Store(statusRunning)
	for _, job := range []*shellProcess{first, second, completed, manual} {
		tool.handler.jobs.Store(job.id, job)
	}
	ctx := tools.WithResourceOwner(t.Context(), firstOwner)
	list, err := tool.handler.ListRunningProcesses(ctx, nil)
	require.NoError(t, err)
	assert.Contains(t, list.Output, "proc_first")
	assert.Contains(t, list.Output, "One-off diagnostic snapshot")
	assert.Contains(t, list.Output, "never poll")
	assert.NotContains(t, list.Output, "proc_second")
	assert.NotContains(t, list.Output, "proc_done")
	assert.NotContains(t, list.Output, "proc_manual")
	inspect, err := tool.handler.InspectRunningProcess(ctx, RunningProcessArgs{ProcessID: first.id})
	require.NoError(t, err)
	assert.Contains(t, inspect.Output, "private")
	assert.Contains(t, inspect.Output, "One-off diagnostic snapshot")
	assert.Contains(t, inspect.Output, "not a wait/poll mechanism")
	assert.Contains(t, inspect.Output, "never poll")
	assert.Contains(t, inspect.Output, "Completion arrives automatically")
	for _, id := range []string{"proc_second", "proc_done", "proc_manual", "unknown"} {
		denied, err := tool.handler.InspectRunningProcess(ctx, RunningProcessArgs{ProcessID: id})
		require.NoError(t, err)
		assert.True(t, denied.IsError)
		assert.NotContains(t, denied.Output, "private")
	}
	unowned, err := tool.handler.ListRunningProcesses(t.Context(), nil)
	require.NoError(t, err)
	assert.True(t, unowned.IsError)
	assert.NotContains(t, unowned.Output, "proc_first")
}

func TestInspectRunningProcessOutputIsBounded(t *testing.T) {
	tool := newTestTool(t)
	owner := tools.NewResourceOwner()
	job := &shellProcess{owner: owner, id: "proc_large", cmd: "build", output: bytes.NewBufferString(strings.Repeat("x", maxShellProcessOutputBytes)), startTime: time.Now(), done: make(chan struct{}), shellOwned: true}
	job.status.Store(statusRunning)
	tool.handler.jobs.Store(job.id, job)
	result, err := tool.handler.InspectRunningProcess(tools.WithResourceOwner(t.Context(), owner), RunningProcessArgs{ProcessID: job.id})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(result.Output), maxShellProcessOutputBytes+512)
	assert.Contains(t, result.Output, "Status: running")
}

func TestKillRunningProcessCancelsAdoptedWork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell process test")
	}
	tool := newTestTool(t)
	owner := tools.NewResourceOwner()
	work := &testShellWork{completed: make(chan string, 1), canceled: make(chan struct{}), closed: make(chan struct{})}
	result := tool.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, testShell, testShellArgs, "echo ready; exec sleep 30", ".", testShellEnv, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.NotEmpty(t, result.JobID)
	ctx := tools.WithResourceOwner(t.Context(), owner)
	killed, err := tool.handler.KillRunningProcess(ctx, RunningProcessArgs{ProcessID: result.JobID})
	require.NoError(t, err)
	require.False(t, killed.IsError, killed.Output)
	assert.Contains(t, killed.Output, "kill only to cancel")
	select {
	case <-work.canceled:
	case <-time.After(time.Second):
		t.Fatal("kill did not cancel completion")
	}
	select {
	case <-work.closed:
	case <-time.After(time.Second):
		t.Fatal("kill did not close retained background work")
	}
	select {
	case <-work.completed:
		t.Fatal("stopped process sent completion")
	default:
	}
	_, exists := tool.handler.jobs.Load(result.JobID)
	assert.False(t, exists, "stopped processes must not remain inspectable")
}

func TestNoRunningProcessesWithoutOwner(t *testing.T) {
	tool := newTestTool(t)
	result, err := tool.handler.ListRunningProcesses(context.Background(), nil)
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "session resource owner")
}
