package shellprocess

import (
	"bytes"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestShellBackgroundReservationsBoundConcurrentJobs(t *testing.T) {
	tool := New()
	owner := tools.NewResourceOwner()
	var slots []func()
	for range maxProcessesPerOwner {
		release, ok := tool.handler.reserveShellSlot(owner)
		require.True(t, ok)
		slots = append(slots, release)
	}
	_, ok := tool.handler.reserveShellSlot(owner)
	require.False(t, ok)
	var active atomic.Int32
	var maxActive atomic.Int32
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			release, ok := tool.handler.reserveShellSlot(tools.NewResourceOwner())
			if !ok {
				return
			}
			current := active.Add(1)
			for old := maxActive.Load(); current > old && !maxActive.CompareAndSwap(old, current); old = maxActive.Load() {
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			release()
		})
	}
	group.Wait()
	require.LessOrEqual(t, maxActive.Load(), int32(maxProcessesPerOwner))
	for _, release := range slots {
		release()
	}
}

func TestShellBackgroundOutputIsBounded(t *testing.T) {
	job := &shellProcess{output: &bytes.Buffer{}}
	writer := &shellCommandOutput{job: job}
	payload := strings.Repeat("x", maxShellProcessOutputBytes+1)
	written, err := writer.Write([]byte(payload))
	require.NoError(t, err)
	require.Equal(t, len(payload), written)
	job.outputMu.RLock()
	retained := job.output.Len()
	job.outputMu.RUnlock()
	require.Equal(t, maxShellProcessOutputBytes, retained)
	require.True(t, writer.truncated)
}

func TestShellBackgroundTimeoutKeepsOriginalDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	tool := newTestTool(t)
	owner := tools.NewResourceOwner()
	ctx := tools.WithResourceOwner(t.Context(), owner)
	work := &timeoutShellWork{complete: make(chan string, 1), closed: make(chan struct{})}
	started := time.Now()
	result := tool.RunShellCommand(ctx, owner, testShell, testShellArgs, "exec sleep 30", ".", testShellEnv, 150*time.Millisecond, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.NotEmpty(t, result.JobID)
	var job *shellProcess
	tool.handler.jobs.Range(func(_ string, candidate *shellProcess) bool {
		if candidate.id == result.JobID {
			job = candidate
		}
		return true
	})
	require.NotNil(t, job)
	select {
	case message := <-work.complete:
		require.Contains(t, message, "timed out after")
	case <-time.After(2 * time.Second):
		t.Fatal("timed-out retained shell job did not complete")
	}
	require.Less(t, time.Since(started), time.Second, "backgrounding must not restart its timeout")
	<-job.done
	require.Equal(t, statusFailed, job.status.Load())
	require.Error(t, job.err)
	if job.process != nil {
		require.NotNil(t, job.process)
	}
}

type timeoutShellWork struct {
	complete chan string
	closed   chan struct{}
	once     sync.Once
}

func (w *timeoutShellWork) Complete(message string) error { w.complete <- message; return nil }
func (*timeoutShellWork) Cancel()                         {}
func (w *timeoutShellWork) Close() error                  { w.once.Do(func() { close(w.closed) }); return nil }

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

type lifecycleShellWork struct {
	adopted  chan tools.BackgroundProcess
	finished chan struct {
		state tools.BackgroundProcessState
		at    time.Time
	}
	complete    chan string
	closed      chan struct{}
	once        sync.Once
	cancelCount atomic.Int32
}

func (w *lifecycleShellWork) AdoptBackgroundProcess(process tools.BackgroundProcess) error {
	w.adopted <- process
	return nil
}

func (*lifecycleShellWork) BackgroundProcessIdentity() (string, string, string) {
	return "session-1", "worker", "tool-call-1"
}

func (w *lifecycleShellWork) FinishBackgroundProcess(state tools.BackgroundProcessState, at time.Time) {
	w.finished <- struct {
		state tools.BackgroundProcessState
		at    time.Time
	}{state, at}
}

func (w *lifecycleShellWork) Complete(message string) error { w.complete <- message; return nil }

func (w *lifecycleShellWork) Cancel() { w.cancelCount.Add(1) }

func (w *lifecycleShellWork) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func TestShellLifecyclePublishesOnlyAdoptedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	tool := newTestTool(t)
	owner := tools.NewResourceOwner()
	work := &lifecycleShellWork{adopted: make(chan tools.BackgroundProcess, 1), finished: make(chan struct {
		state tools.BackgroundProcessState
		at    time.Time
	}, 1), complete: make(chan string, 1), closed: make(chan struct{})}
	result := tool.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, testShell, testShellArgs, "sleep 0.2", "/tmp", testShellEnv, 0, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.NoError(t, result.Err)
	require.NotEmpty(t, result.JobID)
	adopted := <-work.adopted
	require.Equal(t, result.JobID, adopted.ID)
	assert.Equal(t, "session-1", adopted.SessionID)
	assert.Equal(t, "worker", adopted.AgentName)
	assert.Equal(t, "tool-call-1", adopted.ToolCallID)
	assert.Equal(t, "sleep 0.2", adopted.Command)
	assert.Equal(t, "/tmp", adopted.Cwd)
	assert.Equal(t, tools.BackgroundProcessRunning, adopted.State)
	terminal := <-work.finished
	assert.Equal(t, tools.BackgroundProcessCompleted, terminal.state)
	assert.False(t, terminal.at.Before(adopted.StartedAt))
	<-work.complete
	<-work.closed
}

func TestForegroundShellDoesNotPublishBackgroundLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	tool := newTestTool(t)
	owner := tools.NewResourceOwner()
	work := &lifecycleShellWork{adopted: make(chan tools.BackgroundProcess, 1), finished: make(chan struct {
		state tools.BackgroundProcessState
		at    time.Time
	}, 1), complete: make(chan string, 1), closed: make(chan struct{})}
	result := tool.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, testShell, testShellArgs, "printf foreground", "/tmp", testShellEnv, 0, time.Second, tools.NopRuntime{}, work)
	require.NoError(t, result.Err)
	assert.Empty(t, result.JobID)
	assert.NotEmpty(t, result.Output)
	assert.Empty(t, work.adopted)
	assert.Empty(t, work.finished)
	<-work.closed
}

func TestShellLifecycleReportsFailureAndExplicitStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	tool := newTestTool(t)
	for _, tc := range []struct {
		name, command string
		stop          bool
		want          tools.BackgroundProcessState
	}{
		{name: "failure", command: "sleep 0.05; exit 7", want: tools.BackgroundProcessFailed},
		{name: "explicit_stop", command: "exec sleep 30", stop: true, want: tools.BackgroundProcessStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := tools.NewResourceOwner()
			ownerCtx := tools.WithResourceOwner(t.Context(), owner)
			t.Cleanup(func() { _ = tool.StopResourceOwner(ownerCtx) })
			work := &lifecycleShellWork{adopted: make(chan tools.BackgroundProcess, 1), finished: make(chan struct {
				state tools.BackgroundProcessState
				at    time.Time
			}, 1), complete: make(chan string, 1), closed: make(chan struct{})}
			result := tool.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, testShell, testShellArgs, tc.command, "/tmp", testShellEnv, 0, 10*time.Millisecond, tools.NopRuntime{}, work)
			require.NoError(t, result.Err)
			require.NotEmpty(t, result.JobID)
			process := <-work.adopted
			if tc.stop {
				stopped, err := tool.handler.KillRunningProcess(ownerCtx, RunningProcessArgs{ProcessID: result.JobID})
				require.NoError(t, err)
				require.False(t, stopped.IsError, stopped.Output)
			}
			select {
			case terminal := <-work.finished:
				require.Equal(t, tc.want, terminal.state)
				require.False(t, terminal.at.Before(process.StartedAt))
				if tc.stop {
					require.Positive(t, work.cancelCount.Load())
					require.Empty(t, work.complete)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("terminal lifecycle callback missing")
			}
			select {
			case <-work.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("work was not closed")
			}
		})
	}
}

func TestShellOwnerTeardownReportsStoppedLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	tool := newTestTool(t)
	owner := tools.NewResourceOwner()
	ownerCtx := tools.WithResourceOwner(t.Context(), owner)
	t.Cleanup(func() { _ = tool.StopResourceOwner(ownerCtx) })
	work := &lifecycleShellWork{adopted: make(chan tools.BackgroundProcess, 1), finished: make(chan struct {
		state tools.BackgroundProcessState
		at    time.Time
	}, 1), complete: make(chan string, 1), closed: make(chan struct{})}
	result := tool.RunShellCommand(ownerCtx, owner, testShell, testShellArgs, "exec sleep 30", "/tmp", testShellEnv, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.NotEmpty(t, result.JobID)
	process := <-work.adopted
	require.NoError(t, tool.StopResourceOwner(ownerCtx))
	terminal := <-work.finished
	require.Equal(t, tools.BackgroundProcessStopped, terminal.state)
	require.False(t, terminal.at.Before(process.StartedAt))
	require.Positive(t, work.cancelCount.Load())
	require.Empty(t, work.complete)
	<-work.closed
}
