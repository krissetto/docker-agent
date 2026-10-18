package backgroundjobs

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestShellBackgroundReservationsBoundConcurrentJobsAndRetainedHistory(t *testing.T) {
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	owner := tools.NewResourceOwner()
	var slots []func()
	for range maxShellJobsPerOwner {
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
	require.LessOrEqual(t, maxActive.Load(), int32(maxShellJobsPerOwner))
	for _, release := range slots {
		release()
	}
	jobs := tool.handler.jobs
	for i := range maxRetainedShellJobs {
		job := &backgroundJob{id: fmt.Sprintf("shell_%d", i), owner: owner, startTime: time.Now().Add(time.Duration(i) * time.Second), output: &bytes.Buffer{}, done: make(chan struct{}), shellOwned: true}
		job.status.Store(statusCompleted)
		jobs.Store(job.id, job)
	}
	newJob := &backgroundJob{id: "shell_new", owner: owner, startTime: time.Now(), output: nil, done: make(chan struct{})}
	newJob.status.Store(statusRunning)
	require.True(t, tool.handler.retainShellJob(newJob))
	require.Equal(t, maxRetainedShellJobs, jobs.Length())
	_, oldestPresent := jobs.Load("shell_0")
	require.False(t, oldestPresent)
	_, latestPresent := jobs.Load("shell_7")
	require.True(t, latestPresent)
}

func TestShellBackgroundOutputIsBounded(t *testing.T) {
	job := &backgroundJob{output: &bytes.Buffer{}}
	writer := &shellCommandOutput{job: job}
	payload := strings.Repeat("x", maxBackgroundJobOutputBytes+1)
	written, err := writer.Write([]byte(payload))
	require.NoError(t, err)
	require.Equal(t, len(payload), written)
	job.outputMu.RLock()
	retained := job.output.Len()
	job.outputMu.RUnlock()
	require.Equal(t, maxBackgroundJobOutputBytes, retained)
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
	result := tool.RunShellCommand(ctx, owner, tool.handler.shell, tool.handler.shellArgsPrefix, "exec sleep 30", ".", tool.handler.env, 150*time.Millisecond, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.NotEmpty(t, result.JobID)
	var job *backgroundJob
	tool.handler.jobs.Range(func(_ string, candidate *backgroundJob) bool {
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
