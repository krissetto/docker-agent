package backgroundjobs

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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

func TestShellAdoptedJobStopCancelsWorkAndCompletesOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell process test")
	}
	jobs := newTestTool(t)
	owner := tools.NewResourceOwner()
	work := &testShellWork{completed: make(chan string, 1), canceled: make(chan struct{}), closed: make(chan struct{})}
	result := jobs.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, jobs.handler.shell, jobs.handler.shellArgsPrefix, "echo ready; exec sleep 30", ".", jobs.handler.env, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.NotEmpty(t, result.JobID)
	ctx := tools.WithResourceOwner(t.Context(), owner)
	stopped, err := jobs.handler.StopBackgroundJob(ctx, StopBackgroundJobArgs{JobID: result.JobID})
	require.NoError(t, err)
	require.False(t, stopped.IsError, stopped.Output)
	select {
	case <-work.canceled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel retained work")
	}
	select {
	case <-work.closed:
	case <-time.After(time.Second):
		t.Fatal("stop did not release retained work")
	}
	select {
	case <-work.completed:
		t.Fatal("explicitly stopped job sent completion guidance")
	default:
	}
}

type testShellWork struct {
	completed  chan string
	canceled   chan struct{}
	closed     chan struct{}
	cancelOnce sync.Once
	closeOnce  sync.Once
}

func (w *testShellWork) Complete(message string) error { w.completed <- message; return nil }
func (w *testShellWork) Cancel()                       { w.cancelOnce.Do(func() { close(w.canceled) }) }
func (w *testShellWork) Close() error                  { w.closeOnce.Do(func() { close(w.closed) }); return nil }

func TestShellAdoptedProcessGroupStopTerminatesChildAndSuppressesCompletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group test")
	}
	jobs := newTestTool(t)
	owner := tools.NewResourceOwner()
	ctx := tools.WithResourceOwner(t.Context(), owner)
	work := &testShellWork{completed: make(chan string, 1), canceled: make(chan struct{}), closed: make(chan struct{})}
	start := time.Now()
	result := jobs.RunShellCommand(ctx, owner, jobs.handler.shell, jobs.handler.shellArgsPrefix, "sleep 30 & echo ready; wait", ".", jobs.handler.env, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.NotEmpty(t, result.JobID)
	stopped, err := jobs.handler.StopBackgroundJob(ctx, StopBackgroundJobArgs{JobID: result.JobID})
	require.NoError(t, err)
	require.False(t, stopped.IsError, stopped.Output)
	require.Less(t, time.Since(start), time.Second)
	select {
	case <-work.canceled:
	case <-time.After(time.Second):
		t.Fatal("job stop did not cancel completion")
	}
	select {
	case <-work.closed:
	case <-time.After(time.Second):
		t.Fatal("job stop did not release retained work")
	}
	select {
	case <-work.completed:
		t.Fatal("stopped process sent completion")
	default:
	}
}

func TestShellAdoptionRegistersCancellationBeforeStartingProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process marker test")
	}
	jobs := newTestTool(t)
	owner := tools.NewResourceOwner()
	marker := t.TempDir() + "/must-not-start"
	work := &cancelBeforeStartShellWork{closed: make(chan struct{})}
	result := jobs.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, jobs.handler.shell, jobs.handler.shellArgsPrefix, "echo started > "+marker, ".", jobs.handler.env, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.True(t, result.Canceled)
	require.ErrorIs(t, result.Err, context.Canceled)
	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
	select {
	case <-work.closed:
	case <-time.After(time.Second):
		t.Fatal("pre-start cancellation did not close owner")
	}
	jobs.handler.shellSlotsMu.Lock()
	slots := jobs.handler.shellSlots[owner]
	jobs.handler.shellSlotsMu.Unlock()
	require.Zero(t, slots)
}

type cancelBeforeStartShellWork struct {
	closed chan struct{}
	once   sync.Once
}

func (w *cancelBeforeStartShellWork) SetCancel(cancel func()) { cancel() }
func (*cancelBeforeStartShellWork) Complete(string) error {
	return errors.New("canceled work must not complete")
}
func (*cancelBeforeStartShellWork) Cancel()        {}
func (w *cancelBeforeStartShellWork) Close() error { w.once.Do(func() { close(w.closed) }); return nil }

func TestShellCancellationBeforeStartSkipsProcessAndReleasesSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell marker")
	}
	definition := newTestTool(t)
	owner := tools.NewResourceOwner()
	marker := t.TempDir() + "/must-not-start"
	work := &cancelBeforeStartShellWork{closed: make(chan struct{})}
	result := definition.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, definition.handler.shell, definition.handler.shellArgsPrefix, "echo started > "+marker, ".", definition.handler.env, time.Minute, time.Millisecond, tools.NopRuntime{}, work)
	require.True(t, result.Canceled)
	require.ErrorIs(t, result.Err, context.Canceled)
	_, err := os.Stat(marker)
	require.True(t, errors.Is(err, os.ErrNotExist))
	select {
	case <-work.closed:
	case <-time.After(time.Second):
		t.Fatal("canceled pre-start work did not close")
	}
	definition.handler.shellSlotsMu.Lock()
	count := definition.handler.shellSlots[owner]
	definition.handler.shellSlotsMu.Unlock()
	require.Zero(t, count)
}

func TestShellSupervisorCancelWinsAfterLeaderExitAndKillsAnchoredChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process group")
	}
	cmd := exec.Command("/bin/sh", "-c", `sh -c 'trap "" TERM HUP; echo READY; exec sleep 30' & echo $!; read ready; exit 0`)
	cmd.SysProcAttr = platformSpecificShellSysProcAttr()
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	reader := bufio.NewReader(stdout)
	pidLine, err := reader.ReadString('\n')
	require.NoError(t, err)
	childPID, err := strconv.Atoi(strings.TrimSpace(pidLine))
	require.NoError(t, err)
	require.Positive(t, childPID)
	ready, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "READY", strings.TrimSpace(ready))
	pg, err := createProcessGroup(cmd.Process)
	require.NoError(t, err)
	exit, closeObserver, err := observeShellLeaderExit(cmd.Process)
	require.NoError(t, err)
	supervisor := newShellProcessSupervisor(cmd, cmd.Process, pg, exit, closeObserver)
	childState := func() (string, error) {
		out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(childPID)).Output()
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	state, err := childState()
	require.NoError(t, err)
	require.NotEmpty(t, state)
	require.False(t, strings.HasPrefix(state, "Z"))
	_, err = stdin.Write([]byte("go\\n"))
	require.NoError(t, err)
	require.NoError(t, stdin.Close())
	require.Eventually(t, func() bool { return len(exit) > 0 }, time.Second, 10*time.Millisecond, "leader exit observer must report without reaping")
	state, err = childState()
	require.NoError(t, err)
	require.False(t, strings.HasPrefix(state, "Z"))
	require.NoError(t, syscall.Kill(childPID, syscall.SIGHUP))
	time.Sleep(25 * time.Millisecond)
	state, err = childState()
	require.NoError(t, err, "child must ignore SIGHUP before supervisor cancellation")
	require.False(t, strings.HasPrefix(state, "Z"))
	require.NoError(t, syscall.Kill(childPID, syscall.SIGTERM))
	time.Sleep(25 * time.Millisecond)
	state, err = childState()
	require.NoError(t, err, "child must ignore SIGTERM before supervisor cancellation")
	require.False(t, strings.HasPrefix(state, "Z"))
	require.True(t, supervisor.Stop(false), "cancel wins while leader remains an unreaped anchor")
	go supervisor.run()
	select {
	case <-supervisor.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("canceled supervisor failed to finish")
	}
	result := supervisor.result()
	require.True(t, result.canceled)
	require.NoError(t, result.err)
	require.Eventually(t, func() bool {
		state, err := childState()
		if err == nil {
			return state == "" || strings.HasPrefix(state, "Z")
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return true
		}
		t.Errorf("checking child process state: %v", err)
		return false
	}, 2*time.Second, 20*time.Millisecond, "TERM/HUP-ignoring child is killed before leader reaped")
}

func TestShellSupervisorNaturalExitWinsAndNeverSignalsReapedGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process group")
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = platformSpecificShellSysProcAttr()
	require.NoError(t, cmd.Start())
	pg, err := createProcessGroup(cmd.Process)
	require.NoError(t, err)
	exit, closeObserver, err := observeShellLeaderExit(cmd.Process)
	require.NoError(t, err)
	supervisor := newShellProcessSupervisor(cmd, cmd.Process, pg, exit, closeObserver)
	var signals atomic.Int32
	supervisor.termGroup = func(*os.Process, *processGroup) error { signals.Add(1); return nil }
	supervisor.killGroup = func(*os.Process, *processGroup) error { signals.Add(1); return nil }
	go supervisor.run()
	select {
	case <-supervisor.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("natural supervisor did not finish")
	}
	require.NoError(t, supervisor.result().err)
	require.False(t, supervisor.Stop(false), "cancel loses after natural reap")
	require.Zero(t, signals.Load(), "natural exit must never signal a reaped/reusable process group")
}

func TestShellExitObserverSetupFailureTerminatesAndReapsBeforeReturning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix exit observer test")
	}
	definition := newTestTool(t)
	definition.handler.observeShellExit = func(*os.Process) (<-chan shellProcessExit, func(), error) {
		return nil, func() {}, errors.New("observer unavailable")
	}
	owner := tools.NewResourceOwner()
	work := &timeoutShellWork{complete: make(chan string, 1), closed: make(chan struct{})}
	started := time.Now()
	result := definition.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, definition.handler.shell, definition.handler.shellArgsPrefix, "exec sleep 30", ".", definition.handler.env, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.ErrorContains(t, result.Err, "observer unavailable")
	require.Less(t, time.Since(started), time.Second, "failed observer must clean the direct child rather than silently auto-background")
	select {
	case <-work.closed:
	case <-time.After(time.Second):
		t.Fatal("failed observer leaked work ownership")
	}
	definition.handler.shellSlotsMu.Lock()
	slots := definition.handler.shellSlots[owner]
	definition.handler.shellSlotsMu.Unlock()
	require.Zero(t, slots)
}
