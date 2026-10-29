package shellprocess

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

	"github.com/docker/docker-agent/pkg/tools"
)

func TestRunningProcessDiagnosticsEnforceResourceOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell process test")
	}
	manager := newTestTool(t)
	firstOwner, secondOwner := tools.NewResourceOwner(), tools.NewResourceOwner()
	firstCtx := tools.WithResourceOwner(t.Context(), firstOwner)
	secondCtx := tools.WithResourceOwner(t.Context(), secondOwner)
	work1 := &testShellWork{completed: make(chan string, 1), canceled: make(chan struct{}), closed: make(chan struct{})}
	work2 := &testShellWork{completed: make(chan string, 1), canceled: make(chan struct{}), closed: make(chan struct{})}
	firstResult := manager.RunShellCommand(firstCtx, firstOwner, testShell, testShellArgs, "echo private-output; exec sleep 30", ".", testShellEnv, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work1)
	secondResult := manager.RunShellCommand(secondCtx, secondOwner, testShell, testShellArgs, "echo foreign-output; exec sleep 30", ".", testShellEnv, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work2)
	require.NotEmpty(t, firstResult.JobID)
	require.NotEmpty(t, secondResult.JobID)
	t.Cleanup(func() {
		_ = manager.StopResourceOwner(firstCtx)
		_ = manager.StopResourceOwner(secondCtx)
	})
	first, ok := manager.handler.jobs.Load(firstResult.JobID)
	require.True(t, ok)
	_, ok = manager.handler.jobs.Load(secondResult.JobID)
	require.True(t, ok)
	toolsList, err := manager.ShellInspectionTools()
	require.NoError(t, err)
	call := func(ctx context.Context, name, args string) *tools.ToolCallResult {
		t.Helper()
		for _, tool := range toolsList {
			if tool.Name == name {
				result, err := tool.Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: args}}, tools.NopRuntime{})
				require.NoError(t, err)
				return result
			}
		}
		t.Fatalf("tool %q not found", name)
		return nil
	}
	require.Eventually(t, func() bool {
		first.outputMu.RLock()
		defer first.outputMu.RUnlock()
		return strings.Contains(first.output.String(), "private-output")
	}, 5*time.Second, 10*time.Millisecond)
	listed := call(firstCtx, ToolNameListRunningProcesses, `{}`)
	require.Contains(t, listed.Output, firstResult.JobID)
	require.NotContains(t, listed.Output, secondResult.JobID)
	inspected := call(firstCtx, ToolNameInspectRunningProcess, `{"process_id":"`+firstResult.JobID+`"}`)
	require.Contains(t, inspected.Output, "private-output")
	for _, name := range []string{ToolNameInspectRunningProcess, ToolNameKillRunningProcess} {
		denied := call(firstCtx, name, `{"process_id":"`+secondResult.JobID+`"}`)
		require.True(t, denied.IsError)
		require.NotContains(t, denied.Output, "foreign-output")
	}
	killed := call(firstCtx, ToolNameKillRunningProcess, `{"process_id":"`+firstResult.JobID+`"}`)
	require.False(t, killed.IsError, killed.Output)
	select {
	case <-work1.closed:
	case <-time.After(time.Second):
		t.Fatal("killing an owned process did not close background work")
	}
	listed = call(firstCtx, ToolNameListRunningProcesses, `{}`)
	require.NotContains(t, listed.Output, firstResult.JobID, "stopped jobs are not inspectable")
}

func TestRunningProcessDiagnosticsRequireResourceOwner(t *testing.T) {
	manager := newTestTool(t)
	for _, name := range []string{ToolNameListRunningProcesses, ToolNameInspectRunningProcess, ToolNameKillRunningProcess} {
		toolsList, err := manager.ShellInspectionTools()
		require.NoError(t, err)
		var candidate tools.Tool
		for _, tool := range toolsList {
			if tool.Name == name {
				candidate = tool
			}
		}
		require.NotEmpty(t, candidate.Name)
		args := `{}`
		if name != ToolNameListRunningProcesses {
			args = `{"process_id":"proc_1"}`
		}
		result, err := candidate.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: args}}, tools.NopRuntime{})
		require.NoError(t, err)
		require.True(t, result.IsError)
	}
}

func TestShellAdoptedJobStopCancelsWorkAndCompletesOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell process test")
	}
	jobs := newTestTool(t)
	owner := tools.NewResourceOwner()
	work := &testShellWork{completed: make(chan string, 1), canceled: make(chan struct{}), closed: make(chan struct{})}
	result := jobs.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, testShell, testShellArgs, "echo ready; exec sleep 30", ".", testShellEnv, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.NotEmpty(t, result.JobID)
	ctx := tools.WithResourceOwner(t.Context(), owner)
	stopped, err := jobs.handler.KillRunningProcess(ctx, RunningProcessArgs{ProcessID: result.JobID})
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
	result := jobs.RunShellCommand(ctx, owner, testShell, testShellArgs, "sleep 30 & echo ready; wait", ".", testShellEnv, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
	require.NotEmpty(t, result.JobID)
	stopped, err := jobs.handler.KillRunningProcess(ctx, RunningProcessArgs{ProcessID: result.JobID})
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
	result := jobs.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, testShell, testShellArgs, "echo started > "+marker, ".", testShellEnv, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
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
	result := definition.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, testShell, testShellArgs, "echo started > "+marker, ".", testShellEnv, time.Minute, time.Millisecond, tools.NopRuntime{}, work)
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
	result := definition.RunShellCommand(tools.WithResourceOwner(t.Context(), owner), owner, testShell, testShellArgs, "exec sleep 30", ".", testShellEnv, time.Minute, 10*time.Millisecond, tools.NopRuntime{}, work)
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
