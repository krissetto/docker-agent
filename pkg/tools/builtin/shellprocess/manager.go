// Package shellprocess owns processes retained after a shell tool call returns.
package shellprocess

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameListRunningProcesses  = "list_running_processes"
	ToolNameInspectRunningProcess = "inspect_running_process"
	ToolNameKillRunningProcess    = "kill_running_process"

	maxShellProcessOutputBytes = 10 * 1024 * 1024
	maxProcessesPerOwner       = 8
	maxActiveProcesses         = 8

	// BackgroundFollowupGuidance keeps adopted process ownership distinct from model polling.
	BackgroundFollowupGuidance = "Completion output is delivered automatically and wakes/resumes this agent. After adoption, do independent work; if no independent work remains, end your turn without tool calls (no keep-alive needed). Never poll with inspect_running_process, list_running_processes, sleep, or dummy calls. Use inspect/list only once for a concrete diagnostic or user request; use kill only to intentionally cancel, never to end your turn."
	// BackgroundInspectionGuidance presents process inspection as a one-off diagnostic, not a wait mechanism.
	BackgroundInspectionGuidance = "Completion arrives automatically and wakes/resumes this agent; do independent work or end your turn without tool calls; never poll; use inspect/list once only for a concrete diagnostic or user request."
	// BackgroundStopGuidance distinguishes intentional cancellation from ordinary completion.
	BackgroundStopGuidance = "Intentional cancellation only; never kill just to end your turn. Normal completion output arrives automatically."

	// waitDelayAfterJobExit specifies how long cmd.Wait waits for I/O pipes to close
	// after the main process exits. Like the shell tool, this prevents hangs when a
	// background job daemonizes (e.g. `myserver &`) and the grandchild inherits
	// stdout/stderr. If the delay expires, Wait() returns exec.ErrWaitDelay and the
	// pipes are forcefully closed. This truncates captured output from the still-running
	// grandchild and typically kills it with SIGPIPE on its next write. This trade-off
	// keeps the process monitor bounded while shell commands are still
	// monitored internally, and their bounded output is delivered on completion.
	waitDelayAfterJobExit = 1 * time.Second
	gracefulStopTimeout   = 500 * time.Millisecond
	forcedStopTimeout     = 2 * time.Second
)

// Manager owns session-retained shell processes and their bounded lifecycle.
type Manager struct {
	handler *processHandler
}

type processHandler struct {
	jobs             *concurrent.Map[string, *shellProcess]
	shellSlotsMu     sync.Mutex
	shellSlots       map[*tools.ResourceOwner]int
	observeShellExit func(*os.Process) (<-chan shellProcessExit, func(), error)
}

var processIDCounter atomic.Int64

const (
	statusRunning int32 = iota
	statusCompleted
	statusStopped
	statusFailed
)

type shellProcess struct {
	owner            *tools.ResourceOwner
	id               string
	cmd              string
	cwd              string
	process          *os.Process
	processGroup     *processGroup
	outputMu         sync.RWMutex
	output           *bytes.Buffer
	startTime        time.Time
	status           atomic.Int32
	exitCode         int
	err              error
	done             chan struct{}
	stopRequested    atomic.Bool
	timeoutRequested atomic.Bool
	timeoutAfter     time.Duration
	work             tools.BackgroundWork
	releaseSlot      func()
	shellOwned       bool
	processControl   *shellProcessSupervisor
	stopMu           sync.Mutex
}

// limitedWriter wraps a buffer and stops writing after maxSize bytes. It uses
// an external mutex so readers of the underlying buffer can share the same lock.
type limitedWriter struct {
	mu      *sync.RWMutex
	buf     *bytes.Buffer
	written int64
	maxSize int64
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()

	if remaining := lw.maxSize - lw.written; remaining > 0 {
		toWrite := min(int64(len(p)), remaining)
		lw.buf.Write(p[:toWrite]) // bytes.Buffer.Write never errors
		lw.written += toWrite
	}
	return len(p), nil
}

// RunningProcessArgs identifies a currently-running shell process owned by this session.
type RunningProcessArgs struct {
	ProcessID string `json:"process_id" jsonschema:"Process ID returned by the shell tool when a command continues in the background, or from a one-off list_running_processes diagnostic snapshot (not for polling)."`
}

var statusStrings = map[int32]string{
	statusRunning:   "running",
	statusCompleted: "completed",
	statusStopped:   "stopped",
	statusFailed:    "failed",
}

func statusToString(status int32) string {
	if s, ok := statusStrings[status]; ok {
		return s
	}
	return "unknown"
}

func (h *processHandler) finishJob(ctx context.Context, job *shellProcess, cmd *exec.Cmd, err error) {
	defer func() {
		close(job.done)
		if job.work != nil {
			_ = job.work.Close()
		}
		if job.releaseSlot != nil {
			job.releaseSlot()
		}
	}()

	job.outputMu.Lock()

	var newStatus int32
	if job.timeoutRequested.Load() {
		job.exitCode = -1
		newStatus = statusFailed
		job.err = fmt.Errorf("command timed out after %s", job.timeoutAfter)
	} else if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			job.exitCode = exitErr.ExitCode()
		} else {
			job.exitCode = -1
		}
		newStatus = statusFailed
		job.err = err
	} else {
		// Either err == nil (clean exit) or err == exec.ErrWaitDelay (grandchild inherited pipes).
		job.exitCode = cmd.ProcessState.ExitCode()
		if job.exitCode == 0 {
			newStatus = statusCompleted
		} else {
			newStatus = statusFailed
		}
	}

	if job.stopRequested.Load() {
		newStatus = statusStopped
	}
	job.status.Store(newStatus)

	status := job.status.Load()
	exitCode := job.exitCode
	output := job.output.String()
	work := job.work
	job.outputMu.Unlock()
	if lifecycle, ok := work.(tools.BackgroundProcessLifecycle); ok {
		lifecycle.FinishBackgroundProcess(backgroundState(status), time.Now())
	}
	if work != nil && status != statusStopped {
		message := formatShellJobCompletion(job, status, exitCode, output)
		if err := work.Complete(message); err != nil {
			slog.WarnContext(ctx, "Failed to enqueue shell job completion", "job_id", job.id, "error", err)
		}
	} else if work != nil {
		work.Cancel()
	}
	if job.shellOwned {
		h.jobs.Delete(job.id)
	}
}

// ShellCommandResult describes a shell execution that either completed in the
// foreground or was adopted by the background-job owner.
type ShellCommandResult struct {
	JobID    string
	Output   string
	Err      error
	TimedOut bool
	Canceled bool
}

type shellWorkCanceler interface{ SetCancel(func()) }
type shellProcessState uint8

const (
	shellProcessRunning shellProcessState = iota
	shellProcessStopping
	shellProcessReaping
	shellProcessDone
)

type shellProcessExit struct{ err error }
type shellWaitResult struct {
	err                error
	timedOut, canceled bool
}
type shellProcessSupervisor struct {
	mu                 sync.Mutex
	state              shellProcessState
	stop               chan struct{}
	done               chan struct{}
	cmd                *exec.Cmd
	proc               *os.Process
	group              *processGroup
	exit               <-chan shellProcessExit
	closeObserver      func()
	termGroup          func(*os.Process, *processGroup) error
	killGroup          func(*os.Process, *processGroup) error
	timedOut, canceled bool
	err                error
}

func newShellProcessSupervisor(cmd *exec.Cmd, proc *os.Process, group *processGroup, exit <-chan shellProcessExit, closeObserver func()) *shellProcessSupervisor {
	return &shellProcessSupervisor{state: shellProcessRunning, stop: make(chan struct{}), done: make(chan struct{}), cmd: cmd, proc: proc, group: group, exit: exit, closeObserver: closeObserver, termGroup: func(p *os.Process, g *processGroup) error { return terminateShellProcessGroup(p, g, false) }, killGroup: func(p *os.Process, g *processGroup) error { return terminateShellProcessGroup(p, g, true) }}
}
func (s *shellProcessSupervisor) Stop(timedOut bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != shellProcessRunning {
		return false
	}
	s.state = shellProcessStopping
	s.timedOut = timedOut
	s.canceled = !timedOut
	close(s.stop)
	return true
}
func (s *shellProcessSupervisor) Adoptable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == shellProcessRunning
}
func (s *shellProcessSupervisor) Done() <-chan struct{} { return s.done }
func (s *shellProcessSupervisor) result() shellWaitResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return shellWaitResult{s.err, s.timedOut, s.canceled}
}
func (s *shellProcessSupervisor) run() {
	defer close(s.done)
	defer s.closeObserver()
	select {
	case <-s.stop:
		s.stopAndReap()
	case event := <-s.exit:
		if event.err != nil {
			s.Stop(true)
			s.stopAndReap()
			return
		}
		s.mu.Lock()
		if s.state == shellProcessRunning {
			s.state = shellProcessReaping
			s.mu.Unlock()
			s.reap()
		} else {
			s.mu.Unlock()
			s.stopAndReap()
		}
	}
}
func (s *shellProcessSupervisor) stopAndReap() {
	// The exit observer has not reaped the direct child, pinning its group ID.
	_ = s.termGroup(s.proc, s.group)
	time.Sleep(gracefulStopTimeout)
	_ = s.killGroup(s.proc, s.group)
	s.mu.Lock()
	s.state = shellProcessReaping
	s.mu.Unlock()
	s.reap()
}
func (s *shellProcessSupervisor) reap() {
	s.err = s.cmd.Wait()
	s.mu.Lock()
	s.state = shellProcessDone
	s.mu.Unlock()
}

type shellCommandOutput struct {
	job        *shellProcess
	runtime    tools.Runtime
	ctx        context.Context
	foreground atomic.Bool
	written    int64
	truncated  bool
	mu         sync.Mutex
}

func (w *shellCommandOutput) result() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.job.outputMu.RLock()
	defer w.job.outputMu.RUnlock()
	output := w.job.output.String()
	if w.truncated {
		output += "\n\n[Output truncated at 10MB limit]"
	}
	return output
}

func (w *shellCommandOutput) switchToBackground() {
	w.mu.Lock()
	w.foreground.Store(false)
	w.mu.Unlock()
}

func (w *shellCommandOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	remaining := maxShellProcessOutputBytes - w.written
	kept := min(int64(len(p)), remaining)
	w.written += kept
	if kept < int64(len(p)) {
		w.truncated = true
	}
	if kept > 0 {
		w.job.outputMu.Lock()
		w.job.output.Write(p[:kept])
		w.job.outputMu.Unlock()
	}
	if w.foreground.Load() && kept > 0 {
		w.runtime.EmitOutput(w.ctx, string(p[:kept]))
	}
	w.mu.Unlock()
	return len(p), nil
}

func (h *processHandler) reserveShellSlot(owner *tools.ResourceOwner) (func(), bool) {
	h.shellSlotsMu.Lock()
	if h.shellSlots == nil {
		h.shellSlots = make(map[*tools.ResourceOwner]int)
	}
	active := 0
	for _, count := range h.shellSlots {
		active += count
	}
	if h.shellSlots[owner] >= maxProcessesPerOwner || active >= maxActiveProcesses {
		h.shellSlotsMu.Unlock()
		return nil, false
	}
	h.shellSlots[owner]++
	h.shellSlotsMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.shellSlotsMu.Lock()
			h.shellSlots[owner]--
			if h.shellSlots[owner] <= 0 {
				delete(h.shellSlots, owner)
			}
			h.shellSlotsMu.Unlock()
		})
	}, true
}

func (h *processHandler) retainShellJob(job *shellProcess) bool {
	job.shellOwned = true
	h.jobs.Store(job.id, job)
	return true
}

func (h *processHandler) observeLeaderExit(proc *os.Process) (<-chan shellProcessExit, func(), error) {
	if h.observeShellExit != nil {
		return h.observeShellExit(proc)
	}
	return observeShellLeaderExit(proc)
}

// RunShellCommand runs one shell command, returning it normally when it exits
// within foregroundWindow and otherwise retaining the same live process as a
// background job. work must already have been registered with the runtime.
func (t *Manager) RunShellCommand(ctx context.Context, owner *tools.ResourceOwner, shell string, argsPrefix []string, command, cwd string, env []string, timeout, foregroundWindow time.Duration, rt tools.Runtime, work tools.BackgroundWork) ShellCommandResult {
	if owner == nil {
		if work != nil {
			_ = work.Close()
		}
		return ShellCommandResult{Err: errors.New("shell processes require a session resource owner")}
	}
	releaseSlot, reserved := t.handler.reserveShellSlot(owner)
	if !reserved {
		if work != nil {
			_ = work.Close()
			work = nil
		}
		releaseSlot = func() {}
		foregroundWindow = 0
	}
	if !shellBackgroundWaitSupported() && foregroundWindow > 0 {
		foregroundWindow = 0
		if work != nil {
			_ = work.Close()
			work = nil
		}
	}
	cmd := exec.Command(shell, append(argsPrefix, command)...) //nolint:noctx // adopted jobs outlive tool-call cancellation
	cmd.Env, cmd.Dir = env, cwd
	cmd.SysProcAttr, cmd.WaitDelay = platformSpecificShellSysProcAttr(), waitDelayAfterJobExit
	job := &shellProcess{owner: owner, id: fmt.Sprintf("job_%d_%d", time.Now().Unix(), processIDCounter.Add(1)), cmd: command, cwd: cwd, output: &bytes.Buffer{}, startTime: time.Now(), done: make(chan struct{}), work: work, timeoutAfter: timeout, releaseSlot: releaseSlot, shellOwned: true}
	output := &shellCommandOutput{job: job, runtime: rt, ctx: ctx}
	output.foreground.Store(true)
	cmd.Stdout, cmd.Stderr = output, output
	var supervisor *shellProcessSupervisor
	var adoptionMu sync.Mutex
	adoptionCanceled := false
	if canceler, ok := work.(shellWorkCanceler); ok {
		canceler.SetCancel(func() {
			adoptionMu.Lock()
			adoptionCanceled = true
			current := supervisor
			adoptionMu.Unlock()
			job.stopRequested.Store(true)
			if current != nil {
				current.Stop(false)
			}
		})
	}
	adoptionMu.Lock()
	canceledBeforeStart := adoptionCanceled
	adoptionMu.Unlock()
	if canceledBeforeStart {
		if work != nil {
			_ = work.Close()
		}
		releaseSlot()
		return ShellCommandResult{Err: context.Canceled, Canceled: true}
	}
	if err := cmd.Start(); err != nil {
		if work != nil {
			_ = work.Close()
		}
		releaseSlot()
		return ShellCommandResult{Err: fmt.Errorf("Error starting command: %w", err)}
	}
	pg, err := createProcessGroup(cmd.Process)
	if err != nil {
		reapSpawnedChild(cmd, pg)
		if work != nil {
			_ = work.Close()
		}
		releaseSlot()
		return ShellCommandResult{Err: fmt.Errorf("Error creating process group: %w", err)}
	}
	job.process, job.processGroup = cmd.Process, pg
	job.startTime = time.Now()
	job.status.Store(statusRunning)
	exit, closeObserver, observerErr := t.handler.observeLeaderExit(cmd.Process)
	if observerErr != nil {
		_ = terminateShellProcessGroup(cmd.Process, pg, true)
		_ = cmd.Wait()
		if work != nil {
			work.Cancel()
			_ = work.Close()
		}
		releaseSlot()
		return ShellCommandResult{Output: output.result(), Err: fmt.Errorf("observe shell leader exit: %w", observerErr)}
	}
	supervisor = newShellProcessSupervisor(cmd, cmd.Process, pg, exit, closeObserver)
	job.processControl = supervisor
	adoptionMu.Lock()
	if adoptionCanceled {
		supervisor.Stop(false)
	}
	adoptionMu.Unlock()
	go supervisor.run()
	var timeoutTimer *time.Timer
	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timeoutTimer = time.NewTimer(timeout)
		timeoutCh = timeoutTimer.C
		defer timeoutTimer.Stop()
	}
	var foregroundTimer *time.Timer
	var foregroundCh <-chan time.Time
	if foregroundWindow > 0 && work != nil {
		foregroundTimer = time.NewTimer(foregroundWindow)
		foregroundCh = foregroundTimer.C
		defer foregroundTimer.Stop()
	}
	for {
		select {
		case <-supervisor.Done():
			r := supervisor.result()
			if work != nil {
				if r.canceled {
					work.Cancel()
				}
				_ = work.Close()
			}
			releaseSlot()
			return ShellCommandResult{Output: output.result(), Err: r.err, TimedOut: r.timedOut, Canceled: r.canceled}
		case <-ctx.Done():
			supervisor.Stop(errors.Is(ctx.Err(), context.DeadlineExceeded))
			<-supervisor.Done()
			r := supervisor.result()
			if work != nil {
				if r.canceled {
					work.Cancel()
				}
				_ = work.Close()
			}
			releaseSlot()
			return ShellCommandResult{Output: output.result(), Err: ctx.Err(), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), Canceled: r.canceled}
		case <-timeoutCh:
			supervisor.Stop(true)
			<-supervisor.Done()
			r := supervisor.result()
			if work != nil {
				_ = work.Close()
			}
			releaseSlot()
			return ShellCommandResult{Output: output.result(), Err: context.DeadlineExceeded, TimedOut: r.timedOut}
		case <-foregroundCh:
			if !supervisor.Adoptable() {
				continue
			}
			jobID := job.id
			if _, loaded := t.handler.jobs.Load(jobID); loaded {
				job.id = fmt.Sprintf("job_%d_%d", time.Now().Unix(), processIDCounter.Add(1))
			}
			if !t.handler.retainShellJob(job) {
				supervisor.Stop(true)
				<-supervisor.Done()
				if work != nil {
					work.Cancel()
					_ = work.Close()
				}
				releaseSlot()
				return ShellCommandResult{Output: output.result(), Err: errors.New("shell background job limit reached")}
			}
			if lifecycle, ok := work.(tools.BackgroundProcessLifecycle); ok {
				sessionID, agentName, toolCallID := "", "", ""
				if identity, ok := work.(tools.BackgroundProcessIdentity); ok {
					sessionID, agentName, toolCallID = identity.BackgroundProcessIdentity()
				}
				process := tools.BackgroundProcess{ID: job.id, SessionID: sessionID, AgentName: agentName, ToolCallID: toolCallID, Command: job.cmd, Cwd: job.cwd, StartedAt: job.startTime, State: tools.BackgroundProcessRunning}
				if err := lifecycle.AdoptBackgroundProcess(process); err != nil {
					t.handler.jobs.Delete(job.id)
					supervisor.Stop(true)
					<-supervisor.Done()
					if work != nil {
						work.Cancel()
						_ = work.Close()
					}
					releaseSlot()
					return ShellCommandResult{Output: output.result(), Err: fmt.Errorf("register adopted shell process: %w", err)}
				}
			}
			output.switchToBackground()
			go t.handler.monitorShellJob(job, supervisor)
			return ShellCommandResult{JobID: job.id}
		}
	}
}

func (h *processHandler) monitorShellJob(job *shellProcess, supervisor *shellProcessSupervisor) {
	if job.timeoutAfter > 0 {
		timer := time.NewTimer(time.Until(job.startTime.Add(job.timeoutAfter)))
		select {
		case <-supervisor.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
			supervisor.Stop(true)
			<-supervisor.Done()
		}
	} else {
		<-supervisor.Done()
	}
	r := supervisor.result()
	if r.timedOut {
		job.timeoutRequested.Store(true)
	}
	if r.canceled {
		job.stopRequested.Store(true)
	}
	h.finishJob(context.Background(), job, supervisor.cmd, r.err)
}

func backgroundState(status int32) tools.BackgroundProcessState {
	switch status {
	case statusCompleted:
		return tools.BackgroundProcessCompleted
	case statusStopped:
		return tools.BackgroundProcessStopped
	default:
		return tools.BackgroundProcessFailed
	}
}

func formatShellJobCompletion(job *shellProcess, status int32, exitCode int, output string) string {
	var result strings.Builder
	fmt.Fprintf(&result, "Shell command %s with status %s (exit code %d).\n", job.id, statusToString(status), exitCode)
	fmt.Fprintf(&result, "Command: %s\n", job.cmd)
	if job.timeoutRequested.Load() {
		fmt.Fprintf(&result, "Error: command timed out after %s\n", job.timeoutAfter)
	}
	result.WriteString("\nCompletion output is delivered automatically and is not retained after delivery.\n\n--- Untrusted command output ---\n")
	if output == "" {
		result.WriteString("<no output>\n")
	} else {
		result.WriteString(output)
	}
	return builtins.BoundToolResult("shell", "shell_completion", result.String(), "Completion output is delivered automatically and is not retained after delivery.")
}

func (t *Manager) ShellInspectionTools() ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name: ToolNameListRunningProcesses, Category: "shell",
			Description:  "Take a one-off snapshot of shell processes still running after the shell call returned and owned by this session. " + BackgroundInspectionGuidance,
			OutputSchema: tools.MustSchemaFor[string](), Handler: tools.NewHandler(t.handler.ListRunningProcesses),
			Annotations: tools.ToolAnnotations{Title: "List Running Processes", ReadOnlyHint: true}, AddDescriptionParameter: true,
		},
		{
			Name: ToolNameInspectRunningProcess, Category: "shell",
			Description: "Take a one-off diagnostic snapshot of a running shell process owned by this session and its output so far. " + BackgroundInspectionGuidance,
			Parameters:  tools.MustSchemaFor[RunningProcessArgs](), OutputSchema: tools.MustSchemaFor[string](), Handler: tools.NewHandler(t.handler.InspectRunningProcess),
			Annotations: tools.ToolAnnotations{Title: "Inspect Running Process", ReadOnlyHint: true}, AddDescriptionParameter: true,
		},
		{
			Name: ToolNameKillRunningProcess, Category: "shell",
			Description: "Intentionally cancel a running shell process owned by this session. " + BackgroundStopGuidance,
			Parameters:  tools.MustSchemaFor[RunningProcessArgs](), OutputSchema: tools.MustSchemaFor[string](), Handler: tools.NewHandler(t.handler.KillRunningProcess),
			Annotations: tools.ToolAnnotations{Title: "Kill Running Process"}, AddDescriptionParameter: true,
		},
	}, nil
}

func (h *processHandler) ownedRunningShellProcess(ctx context.Context, id string) (*shellProcess, bool) {
	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil || id == "" {
		return nil, false
	}
	job, ok := h.jobs.Load(id)
	return job, ok && job.owner == owner && job.shellOwned && job.status.Load() == statusRunning
}

func (h *processHandler) ListRunningProcesses(ctx context.Context, _ map[string]any) (*tools.ToolCallResult, error) {
	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil {
		return tools.ResultError("Error: running processes require a session resource owner."), nil
	}
	var output strings.Builder
	output.WriteString("One-off diagnostic snapshot (not a wait/poll mechanism). ")
	output.WriteString(BackgroundInspectionGuidance)
	output.WriteString("\n\n")
	count := 0
	h.jobs.Range(func(id string, job *shellProcess) bool {
		if job.owner != owner || !job.shellOwned || job.status.Load() != statusRunning {
			return true
		}
		count++
		fmt.Fprintf(&output, "Process ID: %s\n  Command: %s\n  Runtime: %s\n\n", id, job.cmd, time.Since(job.startTime).Round(time.Second))
		return true
	})
	if count == 0 {
		output.WriteString("No running shell processes found.\n")
	}
	return tools.ResultSuccess(output.String()), nil
}

func (h *processHandler) InspectRunningProcess(ctx context.Context, args RunningProcessArgs) (*tools.ToolCallResult, error) {
	job, ok := h.ownedRunningShellProcess(ctx, args.ProcessID)
	if !ok {
		return tools.ResultError("Running process not found: " + args.ProcessID), nil
	}
	job.outputMu.RLock()
	output := job.output.String()
	job.outputMu.RUnlock()
	var result strings.Builder
	fmt.Fprintf(&result, "One-off diagnostic snapshot (not a wait/poll mechanism). %s\nProcess ID: %s\nCommand: %s\nStatus: running\nRuntime: %s\n\n--- Output so far ---\n", BackgroundInspectionGuidance, job.id, job.cmd, time.Since(job.startTime).Round(time.Second))
	if output == "" {
		result.WriteString("<no output yet>\n")
	} else {
		result.WriteString(output)
	}
	return tools.ResultSuccess(result.String()), nil
}

func (h *processHandler) KillRunningProcess(ctx context.Context, args RunningProcessArgs) (*tools.ToolCallResult, error) {
	job, ok := h.ownedRunningShellProcess(ctx, args.ProcessID)
	if !ok {
		return tools.ResultError("Running process not found: " + args.ProcessID), nil
	}
	if err := stopShellProcess(job); err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	return tools.ResultSuccess("Process " + args.ProcessID + " stopped intentionally. It will not produce completion output; kill only to cancel, never merely to end your turn."), nil
}

func stopShellProcess(job *shellProcess) error {
	job.stopMu.Lock()
	defer job.stopMu.Unlock()
	if job.status.Load() != statusRunning {
		return fmt.Errorf("job %s is not running (current status: %s)", job.id, statusToString(job.status.Load()))
	}

	if job.shellOwned && job.processControl != nil {
		stopAccepted := job.processControl.Stop(false)
		if stopAccepted {
			job.stopRequested.Store(true)
			if job.work != nil {
				job.work.Cancel()
			}
		}
		if !waitForJob(job.done, gracefulStopTimeout+forcedStopTimeout) {
			return fmt.Errorf("timed out waiting for process %s to stop", job.id)
		}
		return nil
	}
	terminate := terminateProcess
	if job.shellOwned {
		terminate = terminateShellProcessGroup
	}
	if err := terminate(job.process, job.processGroup, false); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if job.status.Load() == statusRunning {
			return fmt.Errorf("error stopping process %s: %w", job.id, err)
		}
		return nil
	}
	job.stopRequested.Store(true)
	if job.work != nil {
		job.work.Cancel()
	}
	if !waitForJob(job.done, gracefulStopTimeout) {
		if err := terminate(job.process, job.processGroup, true); err != nil && !errors.Is(err, os.ErrProcessDone) && job.status.Load() == statusRunning {
			return fmt.Errorf("error force-stopping process %s: %w", job.id, err)
		}
		if !waitForJob(job.done, forcedStopTimeout) {
			return fmt.Errorf("timed out waiting for process %s to stop", job.id)
		}
	}

	return nil
}

func waitForJob(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func reapSpawnedChild(cmd *exec.Cmd, pg *processGroup) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = terminateProcess(cmd.Process, pg, false)

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = terminateProcess(cmd.Process, pg, true)
		<-done
	}
}

// New creates a process manager for automatic shell backgrounding.
func New() *Manager {
	return &Manager{handler: &processHandler{jobs: concurrent.NewMap[string, *shellProcess]()}}
}

// Close releases a raw standalone owner using a bounded cleanup context.
func (t *Manager) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return t.CloseAll(ctx)
}

// CloseAll is final raw-owner teardown after every environment lease is released.
// Session retirement must use StopResourceOwner so sibling session jobs survive.
func (t *Manager) CloseAll(ctx context.Context) error {
	var errs []error
	t.handler.jobs.Range(func(_ string, job *shellProcess) bool {
		if job.status.Load() == statusRunning {
			if err := stopShellProcess(job); err != nil {
				errs = append(errs, err)
			}
		}
		select {
		case <-job.done:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return true
		case <-time.After(forcedStopTimeout):
			errs = append(errs, errors.New("timed out waiting for shell process cleanup"))
			return true
		}
		t.handler.jobs.Delete(job.id)
		return true
	})
	return errors.Join(errs...)
}

// ProcessCount reports currently monitored processes for lifecycle tests.
func (t *Manager) ProcessCount() int { return t.handler.jobs.Length() }

// SupportsShellBackground reports whether the host has a race-safe direct-child
// exit observer suitable for process-group cancellation escalation.
func (*Manager) SupportsShellBackground() bool { return shellBackgroundWaitSupported() }

func (t *Manager) StopResourceOwner(ctx context.Context) error {
	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil {
		return nil
	}
	retireErr := t.Stop(ctx)
	t.handler.jobs.Range(func(id string, job *shellProcess) bool {
		if job.owner != owner {
			return true
		}
		timer := time.NewTimer(forcedStopTimeout)
		defer timer.Stop()
		select {
		case <-job.done:
			t.handler.jobs.Delete(id)
		case <-ctx.Done():
			retireErr = errors.Join(retireErr, ctx.Err())
		case <-timer.C:
			retireErr = errors.Join(retireErr, fmt.Errorf("timed out retiring process %s", id))
		}
		return true
	})
	return retireErr
}

func (t *Manager) Stop(ctx context.Context) error {
	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil {
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error)
	t.handler.jobs.Range(func(_ string, job *shellProcess) bool {
		if job.owner == owner && job.status.Load() == statusRunning {
			wg.Go(func() {
				if err := stopShellProcess(job); err != nil {
					errs <- err
				}
			})
		}
		return true
	})
	go func() {
		wg.Wait()
		close(errs)
	}()

	var stopErr error
	for err := range errs {
		stopErr = errors.Join(stopErr, err)
	}
	return stopErr
}
