package shellprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/require"
)

type liveBatch struct {
	offset int64
	output string
}
type liveShellWork struct {
	tools.NopRuntime
	batches  chan liveBatch
	finished chan tools.BackgroundProcess
	complete chan string
	mu       sync.Mutex
	order    []string
}

func (*liveShellWork) BackgroundProcessIdentity() (string, string, string) {
	return "session", "agent", "call"
}
func (*liveShellWork) AdoptBackgroundProcess(tools.BackgroundProcess) error            { return nil }
func (*liveShellWork) FinishBackgroundProcess(tools.BackgroundProcessState, time.Time) {}
func (w *liveShellWork) FinishBackgroundProcessResult(state tools.BackgroundProcessState, at time.Time, code int, timeout bool) {
	w.mu.Lock()
	w.order = append(w.order, "finished")
	w.mu.Unlock()
	w.finished <- tools.BackgroundProcess{State: state, FinishedAt: at, ExitCode: code, TimedOut: timeout}
}
func (w *liveShellWork) EmitBackgroundProcessOutput(offset int64, output string) {
	w.mu.Lock()
	w.order = append(w.order, "output")
	w.mu.Unlock()
	w.batches <- liveBatch{offset, output}
}
func (w *liveShellWork) Complete(s string) error { w.complete <- s; return nil }
func (*liveShellWork) Cancel()                   {}
func (*liveShellWork) Close() error              { return nil }
func newLiveWork() *liveShellWork {
	return &liveShellWork{batches: make(chan liveBatch, 100), finished: make(chan tools.BackgroundProcess, 1), complete: make(chan string, 1)}
}

func TestShellLiveOutputOffsetsAndFinalFlush(t *testing.T) {
	if !shellBackgroundWaitSupported() {
		t.Skip("requires background shell observer")
	}
	manager := newTestTool(t)
	owner := tools.NewResourceOwner()
	ctx := tools.WithResourceOwner(t.Context(), owner)
	gate := filepath.Join(t.TempDir(), "release")
	work := newLiveWork()
	command := "printf before; while [ ! -f '" + gate + "' ]; do sleep 0.01; done; printf live; sleep 0.25; printf final"
	result := manager.RunShellCommand(ctx, owner, testShell, testShellArgs, command, ".", testShellEnv, 0, 50*time.Millisecond, tools.NopRuntime{}, work)
	require.NotEmpty(t, result.JobID)
	require.NoError(t, os.WriteFile(gate, nil, 0600))
	select {
	case batch := <-work.batches:
		require.Equal(t, liveBatch{6, "live"}, batch)
	case <-time.After(2 * time.Second):
		t.Fatal("missing live output")
	}
	select {
	case terminal := <-work.finished:
		require.Equal(t, 0, terminal.ExitCode)
		require.False(t, terminal.TimedOut)
	case <-time.After(2 * time.Second):
		t.Fatal("missing completion")
	}
	require.Equal(t, liveBatch{10, "final"}, <-work.batches)
	work.mu.Lock()
	require.Equal(t, []string{"output", "output", "finished"}, work.order)
	work.mu.Unlock()
	parsed, ok := tools.ParseShellCompletion(<-work.complete)
	require.True(t, ok)
	require.Equal(t, result.JobID, parsed.ProcessID)
	require.Equal(t, "beforelivefinal", parsed.Output)
	snapshot, err := manager.BackgroundProcessOutput(ctx, result.JobID, 5)
	require.NoError(t, err)
	require.Equal(t, "final", snapshot.Output)
	require.EqualValues(t, 10, snapshot.Offset)
	require.EqualValues(t, 15, snapshot.Total)
	_, err = manager.BackgroundProcessOutput(tools.WithResourceOwner(t.Context(), tools.NewResourceOwner()), result.JobID, 5)
	require.ErrorIs(t, err, tools.ErrBackgroundProcessNotFound)
}

func TestShellLiveNoisyOutputIsBounded(t *testing.T) {
	work := newLiveWork()
	job := &shellProcess{owner: tools.NewResourceOwner(), id: "job", output: &bytes.Buffer{}, work: work}
	writer := &shellCommandOutput{job: job}
	_, err := writer.Write([]byte(strings.Repeat("x", maxShellProcessOutputBytes+1)))
	require.NoError(t, err)
	require.Len(t, writer.pending, maxBackgroundOutputBatchBytes)
	require.True(t, job.truncated)
	writer.flushBackground()
	batch := <-work.batches
	require.Len(t, batch.output, maxBackgroundOutputBatchBytes)
	require.EqualValues(t, maxShellProcessOutputBytes-maxBackgroundOutputBatchBytes, batch.offset)
	manager := New()
	manager.handler.jobs.Store(job.id, job)
	snapshot, err := manager.BackgroundProcessOutput(tools.WithResourceOwner(context.Background(), job.owner), job.id, 1000000)
	require.NoError(t, err)
	require.Len(t, snapshot.Output, 64*1024)
	require.True(t, snapshot.Truncated)
	require.EqualValues(t, maxShellProcessOutputBytes, snapshot.Total)
}

func TestShellTerminalExitDetails(t *testing.T) {
	if !shellBackgroundWaitSupported() {
		t.Skip("requires background shell observer")
	}
	for _, tc := range []struct {
		name, cmd string
		timeout   time.Duration
		stop      bool
		code      int
		state     tools.BackgroundProcessState
	}{
		{name: "failure", cmd: "sleep 0.05; exit 7", code: 7, state: tools.BackgroundProcessFailed},
		{name: "timeout", cmd: "exec sleep 30", timeout: 50 * time.Millisecond, code: -1, state: tools.BackgroundProcessFailed},
		{name: "stop", cmd: "exec sleep 30", stop: true, code: -1, state: tools.BackgroundProcessStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newTestTool(t)
			owner := tools.NewResourceOwner()
			ctx := tools.WithResourceOwner(t.Context(), owner)
			work := newLiveWork()
			result := manager.RunShellCommand(ctx, owner, testShell, testShellArgs, tc.cmd, ".", testShellEnv, tc.timeout, 10*time.Millisecond, tools.NopRuntime{}, work)
			require.NotEmpty(t, result.JobID)
			if tc.stop {
				_, err := manager.handler.KillRunningProcess(ctx, RunningProcessArgs{ProcessID: result.JobID})
				require.NoError(t, err)
			}
			select {
			case terminal := <-work.finished:
				require.Equal(t, tc.code, terminal.ExitCode)
				require.Equal(t, tc.state, terminal.State)
				require.Equal(t, tc.timeout > 0, terminal.TimedOut)
			case <-time.After(2 * time.Second):
				t.Fatal("missing terminal")
			}
		})
	}
}

func TestShellOutputSnapshotExpiryAndRetentionBounds(t *testing.T) {
	manager := New()
	owner := tools.NewResourceOwner()
	ctx := tools.WithResourceOwner(t.Context(), owner)
	for i := range maxRetainedCompletedProcesses + 5 {
		job := &shellProcess{id: fmt.Sprintf("job-%d", i), owner: owner, output: bytes.NewBufferString("output"), finishedAt: time.Now()}
		job.status.Store(statusCompleted)
		manager.handler.jobs.Store(job.id, job)
	}
	manager.handler.pruneCompletedJobs()
	require.Less(t, manager.handler.jobs.Length(), maxRetainedCompletedProcesses)
	expired := &shellProcess{id: "expired", owner: owner, output: bytes.NewBufferString("output"), finishedAt: time.Now().Add(-completedOutputRetention)}
	expired.status.Store(statusCompleted)
	manager.handler.jobs.Store(expired.id, expired)
	_, err := manager.BackgroundProcessOutput(ctx, expired.id, 1)
	require.ErrorIs(t, err, tools.ErrBackgroundProcessNotFound)
	manager.handler.pruneCompletedJobs()
	_, found := manager.handler.jobs.Load(expired.id)
	require.False(t, found)
}

func TestShellFinishedNoisyOutputRetainsOnlySnapshotTail(t *testing.T) {
	manager := New()
	owner := tools.NewResourceOwner()
	work := newLiveWork()
	job := &shellProcess{id: "job", owner: owner, output: &bytes.Buffer{}, work: work, done: make(chan struct{}), shellOwned: true}
	writer := &shellCommandOutput{job: job}
	_, err := writer.Write([]byte(strings.Repeat("x", maxShellProcessOutputBytes+1)))
	require.NoError(t, err)
	writer.flushBackground()
	batch := <-work.batches
	manager.handler.jobs.Store(job.id, job)
	manager.handler.finishJob(t.Context(), job, nil, errors.New("failed"))
	<-work.complete
	require.LessOrEqual(t, job.output.Len(), maxBackgroundOutputSnapshotBytes)
	require.LessOrEqual(t, job.output.Cap(), maxBackgroundOutputSnapshotBytes)
	snapshot, err := manager.BackgroundProcessOutput(tools.WithResourceOwner(t.Context(), owner), job.id, 0)
	require.NoError(t, err)
	require.True(t, snapshot.Truncated)
	require.EqualValues(t, maxShellProcessOutputBytes, snapshot.Total)
	require.Equal(t, snapshot.Total, snapshot.Offset+int64(len(snapshot.Output)))
	require.Equal(t, snapshot.Total, batch.offset+int64(len(batch.output)))
	require.EqualValues(t, maxShellProcessOutputBytes-maxBackgroundOutputSnapshotBytes, snapshot.Offset)
	small, err := manager.BackgroundProcessOutput(tools.WithResourceOwner(t.Context(), owner), job.id, 15)
	require.NoError(t, err)
	require.Equal(t, snapshot.Total, small.Total)
	require.Equal(t, small.Total, small.Offset+int64(len(small.Output)))
}

type interestedShellWork struct {
	*liveShellWork
	interested bool
}

func (w *interestedShellWork) BackgroundProcessOutputInterested() bool { return w.interested }

func TestShellLiveOutputInterestSkipsBatching(t *testing.T) {
	work := &interestedShellWork{liveShellWork: newLiveWork()}
	job := &shellProcess{owner: tools.NewResourceOwner(), id: "job", output: &bytes.Buffer{}, work: work}
	writer := &shellCommandOutput{job: job}
	for range 100 {
		_, err := writer.Write([]byte(strings.Repeat("x", 1024)))
		require.NoError(t, err)
	}
	require.Empty(t, writer.pending)
	writer.flushBackground()
	require.Empty(t, work.batches)
	work.interested = true
	_, err := writer.Write([]byte("live"))
	require.NoError(t, err)
	writer.flushBackground()
	require.Equal(t, liveBatch{102400, "live"}, <-work.batches)
	_, err = writer.Write([]byte("discard"))
	require.NoError(t, err)
	work.interested = false
	writer.flushBackground()
	require.Empty(t, writer.pending)
	require.Empty(t, work.batches)
	require.Equal(t, int64(102411), writer.written)
}

func TestShellUninterestedOutputStaysInSnapshotForReopen(t *testing.T) {
	manager := New()
	owner := tools.NewResourceOwner()
	ctx := tools.WithResourceOwner(t.Context(), owner)
	work := &interestedShellWork{liveShellWork: newLiveWork()}
	job := &shellProcess{owner: owner, id: "job_1_1", output: bytes.NewBufferString("pre-handoff\n"), work: work, shellOwned: true}
	job.status.Store(statusRunning)
	manager.handler.jobs.Store(job.id, job)
	writer := &shellCommandOutput{job: job, written: int64(job.output.Len())}

	// Every viewer collapsed: no live batches, but the per-job buffer keeps the bytes.
	_, err := writer.Write([]byte("while-collapsed\n"))
	require.NoError(t, err)
	writer.flushBackground()
	require.Empty(t, work.batches)

	snapshot, err := manager.BackgroundProcessOutput(ctx, job.id, 0)
	require.NoError(t, err)
	require.Equal(t, tools.BackgroundProcessRunning, snapshot.State)
	require.Equal(t, "pre-handoff\nwhile-collapsed\n", snapshot.Output)
	require.Equal(t, int64(0), snapshot.Offset)
	require.Equal(t, int64(len(snapshot.Output)), snapshot.Total)

	// Reopened: live batches continue exactly where the catch-up tail ended.
	work.interested = true
	_, err = writer.Write([]byte("after-reopen\n"))
	require.NoError(t, err)
	writer.flushBackground()
	require.Equal(t, liveBatch{snapshot.Total, "after-reopen\n"}, <-work.batches)

	_, err = manager.BackgroundProcessOutput(tools.WithResourceOwner(t.Context(), tools.NewResourceOwner()), job.id, 0)
	require.ErrorIs(t, err, tools.ErrBackgroundProcessNotFound)
}
