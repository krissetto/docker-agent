// Package backgroundjobs exposes the built-in background_jobs toolset.
package backgroundjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	// ToolNameRunBackgroundJob starts a shell command asynchronously.
	ToolNameRunBackgroundJob = "run_background_job"
	// ToolNameListBackgroundJobs lists known background jobs.
	ToolNameListBackgroundJobs = "list_background_jobs"
	// ToolNameViewBackgroundJob renders one background job's status and output.
	ToolNameViewBackgroundJob = "view_background_job"
	// ToolNameStopBackgroundJob stops a running background job.
	ToolNameStopBackgroundJob = "stop_background_job"
	// ToolNameWaitBackgroundJob waits for a background job to finish.
	ToolNameWaitBackgroundJob = "wait_background_job"

	maxBackgroundJobOutputBytes = 10 * 1024 * 1024
	maxShellJobsPerOwner        = 8
	maxRetainedShellJobs        = 8

	// waitDelayAfterJobExit specifies how long cmd.Wait waits for I/O pipes to close
	// after the main process exits. Like the shell tool, this prevents hangs when a
	// background job daemonizes (e.g. `myserver &`) and the grandchild inherits
	// stdout/stderr. If the delay expires, Wait() returns exec.ErrWaitDelay and the
	// pipes are forcefully closed. This truncates captured output from the still-running
	// grandchild and typically kills it with SIGPIPE on its next write. This trade-off
	// is necessary: run_background_job is designed to exit its direct child quickly
	// so the agent can continue its turn. If a user needs a permanently-running job
	// that survives indefinitely while logging, they should redirect its output
	// (e.g. `myserver > log.txt 2>&1 &`).
	waitDelayAfterJobExit = 1 * time.Second
	gracefulStopTimeout   = 500 * time.Millisecond
	forcedStopTimeout     = 2 * time.Second
)

// ToolSet manages long-running shell commands.
type ToolSet struct {
	handler *backgroundJobsHandler
}

// Verify interface compliance
var (
	_ tools.ToolSet              = (*ToolSet)(nil)
	_ tools.Startable            = (*ToolSet)(nil)
	_ tools.Instructable         = (*ToolSet)(nil)
	_ tools.ResourceOwnerStopper = (*ToolSet)(nil)
)

type backgroundJobsHandler struct {
	shell            string
	shellArgsPrefix  []string
	env              []string
	workingDir       string
	jobs             *concurrent.Map[string, *backgroundJob]
	jobCounter       atomic.Int64
	shellSlotsMu     sync.Mutex
	shellSlots       map[*tools.ResourceOwner]int
	recall           bool
	observeShellExit func(*os.Process) (<-chan shellProcessExit, func(), error)
}

const (
	statusRunning int32 = iota
	statusCompleted
	statusStopped
	statusFailed
)

type backgroundJob struct {
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
	rt               tools.Runtime
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

// RunBackgroundJobArgs contains the parameters for run_background_job.
type RunBackgroundJobArgs struct {
	Cmd string `json:"cmd" jsonschema:"Shell command to run in background"`
	Cwd string `json:"cwd,omitempty" jsonschema:"Working directory (default \".\")"`
}

// RunBackgroundJobRecallArgs contains the parameters for run_background_job when recall is enabled.
type RunBackgroundJobRecallArgs struct {
	Cmd    string `json:"cmd" jsonschema:"Shell command to run in background"`
	Cwd    string `json:"cwd,omitempty" jsonschema:"Working directory (default \".\")"`
	Recall bool   `json:"recall,omitempty" jsonschema:"Ask to be recalled with a steering message when the background job finishes"`
}

// UnmarshalJSON accepts both "cmd" (canonical) and "command" (common alias),
// resolved by [safety.CommandArg] over an exact-key map so the executed
// command is the one the runtime classified (see shell.RunShellArgs).
func (a *RunBackgroundJobArgs) UnmarshalJSON(data []byte) error {
	var raw struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	cmd, err := commandField(data)
	if err != nil {
		return err
	}
	a.Cmd = cmd
	a.Cwd = raw.Cwd
	return nil
}

// UnmarshalJSON accepts both "cmd" (canonical) and "command" (common alias),
// resolved like [RunBackgroundJobArgs].
func (a *RunBackgroundJobRecallArgs) UnmarshalJSON(data []byte) error {
	var raw struct {
		Cwd    string `json:"cwd"`
		Recall bool   `json:"recall"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	cmd, err := commandField(data)
	if err != nil {
		return err
	}
	a.Cmd = cmd
	a.Cwd = raw.Cwd
	a.Recall = raw.Recall
	return nil
}

func commandField(data []byte) (string, error) {
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return "", err
	}
	cmd, _ := safety.CommandArg(fields)
	return cmd, nil
}

// ViewBackgroundJobArgs contains the parameters for view_background_job.
type ViewBackgroundJobArgs struct {
	JobID string `json:"job_id" jsonschema:"Background job ID"`
}

// StopBackgroundJobArgs contains the parameters for stop_background_job.
type StopBackgroundJobArgs struct {
	JobID string `json:"job_id" jsonschema:"Background job ID"`
}

// WaitBackgroundJobArgs contains the parameters for wait_background_job.
type WaitBackgroundJobArgs struct {
	JobID   string `json:"job_id" jsonschema:"Background job ID"`
	Timeout int    `json:"timeout,omitempty" jsonschema:"Maximum seconds to wait (default 60). Returns current output with a timeout notice if the job is still running when the limit is reached."`
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

type runBackgroundJobParams struct {
	Cmd    string
	Cwd    string
	Recall bool
}

func (h *backgroundJobsHandler) RunBackgroundJob(ctx context.Context, params RunBackgroundJobArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
	return h.runBackgroundJob(ctx, rt, runBackgroundJobParams{Cmd: params.Cmd, Cwd: params.Cwd})
}

func (h *backgroundJobsHandler) RunBackgroundJobWithRecall(ctx context.Context, params RunBackgroundJobRecallArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
	return h.runBackgroundJob(ctx, rt, runBackgroundJobParams(params))
}

func (h *backgroundJobsHandler) runBackgroundJob(ctx context.Context, rt tools.Runtime, params runBackgroundJobParams) (*tools.ToolCallResult, error) {
	if strings.TrimSpace(params.Cmd) == "" {
		return tools.ResultError(`Error: missing or empty "cmd" parameter. Pass the shell command as {"cmd": "..."}.`), nil
	}

	if params.Recall {
		if !h.recall {
			return tools.ResultError(`Error: "recall" is not enabled for this background_jobs toolset. Set recall: true on the background_jobs toolset before requesting recall.`), nil
		}
		if !rt.Supports(tools.CapabilityRecall) {
			return tools.ResultError(`Error: recall requested but the host does not support recall.`), nil
		}
	}

	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil {
		return tools.ResultError("Error: background jobs require a session resource owner."), nil
	}

	counter := h.jobCounter.Add(1)
	jobID := fmt.Sprintf("job_%d_%d", time.Now().Unix(), counter)

	cmd := exec.Command(h.shell, append(h.shellArgsPrefix, params.Cmd)...) //nolint:noctx // background jobs intentionally outlive the request context
	cmd.Env = h.env
	cmd.Dir = h.resolveWorkDir(params.Cwd)
	cmd.SysProcAttr = platformSpecificSysProcAttr()
	cmd.WaitDelay = waitDelayAfterJobExit

	job := &backgroundJob{
		owner:     owner,
		id:        jobID,
		cmd:       params.Cmd,
		cwd:       params.Cwd,
		output:    &bytes.Buffer{},
		startTime: time.Now(),
		done:      make(chan struct{}),
	}
	if params.Recall {
		job.rt = rt
	}

	lw := &limitedWriter{mu: &job.outputMu, buf: job.output, maxSize: maxBackgroundJobOutputBytes}
	cmd.Stdout = lw
	cmd.Stderr = lw

	if err := cmd.Start(); err != nil {
		return tools.ResultError(fmt.Sprintf("Error starting background command: %s", err)), nil
	}

	pg, err := createProcessGroup(cmd.Process)
	if err != nil {
		reapSpawnedChild(cmd, pg)
		return tools.ResultError(fmt.Sprintf("Error creating process group: %s", err)), nil
	}

	job.process = cmd.Process
	job.processGroup = pg
	job.status.Store(statusRunning)
	h.jobs.Store(jobID, job)

	go h.monitorJob(context.WithoutCancel(ctx), job, cmd)

	var recallStatus string
	if params.Recall {
		recallStatus = "\nRecall: requested; a steering message will be sent when the job finishes."
	}

	return tools.ResultSuccess(fmt.Sprintf("Background job started with ID: %s\nCommand: %s\nWorking directory: %s%s",
		jobID, params.Cmd, params.Cwd, recallStatus)), nil
}

func (h *backgroundJobsHandler) monitorJob(ctx context.Context, job *backgroundJob, cmd *exec.Cmd) {
	h.finishJob(ctx, job, cmd, cmd.Wait())
}

func (h *backgroundJobsHandler) finishJob(ctx context.Context, job *backgroundJob, cmd *exec.Cmd, err error) {
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
	rt := job.rt
	work := job.work
	job.outputMu.Unlock()
	if work != nil && status != statusStopped {
		message := formatShellJobCompletion(job, status, exitCode, output)
		if err := work.Complete(message); err != nil {
			slog.WarnContext(ctx, "Failed to enqueue shell job completion", "job_id", job.id, "error", err)
		}
	} else if work != nil {
		work.Cancel()
	}
	if rt != nil {
		message := formatBackgroundJobRecall(job, status, exitCode, output)
		if err := rt.Recall(ctx, message); err != nil {
			slog.WarnContext(ctx, "Failed to enqueue background job recall", "job_id", job.id, "error", err)
		}
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
	job        *backgroundJob
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
	remaining := maxBackgroundJobOutputBytes - w.written
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

func (h *backgroundJobsHandler) reserveShellSlot(owner *tools.ResourceOwner) (func(), bool) {
	h.shellSlotsMu.Lock()
	if h.shellSlots == nil {
		h.shellSlots = make(map[*tools.ResourceOwner]int)
	}
	active := 0
	for _, count := range h.shellSlots {
		active += count
	}
	if h.shellSlots[owner] >= maxShellJobsPerOwner || active >= maxRetainedShellJobs {
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

func (h *backgroundJobsHandler) retainShellJob(job *backgroundJob) bool {
	h.shellSlotsMu.Lock()
	defer h.shellSlotsMu.Unlock()
	var shellJobs []*backgroundJob
	h.jobs.Range(func(_ string, candidate *backgroundJob) bool {
		if candidate.shellOwned {
			shellJobs = append(shellJobs, candidate)
		}
		return true
	})
	for len(shellJobs) >= maxRetainedShellJobs {
		oldest := -1
		for i, candidate := range shellJobs {
			if candidate.status.Load() == statusRunning {
				continue
			}
			if oldest < 0 || candidate.startTime.Before(shellJobs[oldest].startTime) {
				oldest = i
			}
		}
		if oldest < 0 {
			return false
		}
		h.jobs.Delete(shellJobs[oldest].id)
		shellJobs = append(shellJobs[:oldest], shellJobs[oldest+1:]...)
	}
	job.shellOwned = true
	h.jobs.Store(job.id, job)
	return true
}

func (h *backgroundJobsHandler) observeLeaderExit(proc *os.Process) (<-chan shellProcessExit, func(), error) {
	if h.observeShellExit != nil {
		return h.observeShellExit(proc)
	}
	return observeShellLeaderExit(proc)
}

// RunShellCommand runs one shell command, returning it normally when it exits
// within foregroundWindow and otherwise retaining the same live process as a
// background job. work must already have been registered with the runtime.
func (t *ToolSet) RunShellCommand(ctx context.Context, owner *tools.ResourceOwner, shell string, argsPrefix []string, command, cwd string, env []string, timeout, foregroundWindow time.Duration, rt tools.Runtime, work tools.BackgroundWork) ShellCommandResult {
	if owner == nil {
		if work != nil {
			_ = work.Close()
		}
		return ShellCommandResult{Err: errors.New("shell jobs require a session resource owner")}
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
	cmd.Env, cmd.Dir = env, t.handler.resolveWorkDir(cwd)
	cmd.SysProcAttr, cmd.WaitDelay = platformSpecificShellSysProcAttr(), waitDelayAfterJobExit
	job := &backgroundJob{owner: owner, id: fmt.Sprintf("job_%d_%d", time.Now().Unix(), t.handler.jobCounter.Add(1)), cmd: command, cwd: cwd, output: &bytes.Buffer{}, startTime: time.Now(), done: make(chan struct{}), work: work, timeoutAfter: timeout, releaseSlot: releaseSlot, shellOwned: true}
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
				job.id = fmt.Sprintf("job_%d_%d", time.Now().Unix(), t.handler.jobCounter.Add(1))
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
			output.switchToBackground()
			go t.handler.monitorShellJob(job, supervisor)
			return ShellCommandResult{JobID: job.id}
		}
	}
}

func (h *backgroundJobsHandler) monitorShellJob(job *backgroundJob, supervisor *shellProcessSupervisor) {
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

func formatShellJobCompletion(job *backgroundJob, status int32, exitCode int, output string) string {
	var result strings.Builder
	fmt.Fprintf(&result, "Background shell command %s with status %s (exit code %d).\n", job.id, statusToString(status), exitCode)
	fmt.Fprintf(&result, "Command: %s\n", job.cmd)
	if job.timeoutRequested.Load() {
		fmt.Fprintf(&result, "Error: command timed out after %s\n", job.timeoutAfter)
	}
	result.WriteString("\n--- Untrusted command output ---\n")
	if output == "" {
		result.WriteString("<no output>\n")
	} else {
		result.WriteString(output)
	}
	return builtins.BoundToolResult("background_jobs", ToolNameViewBackgroundJob, result.String(), "Inspect retained output with view_background_job.")
}

type inspectionToolSet struct{ owner *ToolSet }

func (t *inspectionToolSet) Unwrap() tools.ToolSet { return t.owner }
func (t *inspectionToolSet) Instructions() string {
	return "Use list_background_jobs and view_background_job to inspect shell commands that continued in the background. Use stop_background_job to cancel one."
}
func (t *inspectionToolSet) Tools(ctx context.Context) ([]tools.Tool, error) {
	return t.owner.ShellInspectionTools()
}

func (t *ToolSet) InspectionToolSet() tools.ToolSet { return &inspectionToolSet{owner: t} }

// ShellInspectionTools returns only management tools for shell-adopted jobs.
// It deliberately omits run_background_job to avoid exposing a duplicate start API.
func (t *ToolSet) ShellInspectionTools() ([]tools.Tool, error) {
	all, err := t.Tools(context.Background())
	if err != nil {
		return nil, err
	}
	var selected []tools.Tool
	for _, tool := range all {
		switch tool.Name {
		case ToolNameListBackgroundJobs:
			tool.Handler = tools.NewHandler(t.handler.ListShellBackgroundJobs)
			selected = append(selected, tool)
		case ToolNameViewBackgroundJob:
			tool.Handler = tools.NewHandler(t.handler.ViewShellBackgroundJob)
			selected = append(selected, tool)
		case ToolNameWaitBackgroundJob:
			tool.Handler = tools.NewHandler(t.handler.WaitShellBackgroundJob)
			selected = append(selected, tool)
		case ToolNameStopBackgroundJob:
			tool.Handler = tools.NewHandler(t.handler.StopShellBackgroundJob)
			selected = append(selected, tool)
		}
	}
	return selected, nil
}

func (h *backgroundJobsHandler) ListBackgroundJobs(ctx context.Context, _ map[string]any) (*tools.ToolCallResult, error) {
	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil {
		return tools.ResultError("Error: background jobs require a session resource owner."), nil
	}
	var output strings.Builder
	output.WriteString("Background Jobs:\n\n")

	jobCount := 0
	h.jobs.Range(func(jobID string, job *backgroundJob) bool {
		if job.owner != owner {
			return true
		}
		jobCount++
		status := job.status.Load()
		elapsed := time.Since(job.startTime).Round(time.Second)

		fmt.Fprintf(&output, "ID: %s\n", jobID)
		fmt.Fprintf(&output, "  Command: %s\n", job.cmd)
		fmt.Fprintf(&output, "  Status: %s\n", statusToString(status))
		fmt.Fprintf(&output, "  Runtime: %s\n", elapsed)
		if status != statusRunning {
			job.outputMu.RLock()
			fmt.Fprintf(&output, "  Exit Code: %d\n", job.exitCode)
			job.outputMu.RUnlock()
		}
		output.WriteString("\n")
		return true
	})

	if jobCount == 0 {
		output.WriteString("No background jobs found.\n")
	}

	return tools.ResultSuccess(output.String()), nil
}

func renderBackgroundJob(job *backgroundJob) string {
	status := job.status.Load()

	job.outputMu.RLock()
	output := job.output.String()
	exitCode := job.exitCode
	job.outputMu.RUnlock()

	var result strings.Builder
	fmt.Fprintf(&result, "Job ID: %s\n", job.id)
	fmt.Fprintf(&result, "Command: %s\n", job.cmd)
	fmt.Fprintf(&result, "Status: %s\n", statusToString(status))
	fmt.Fprintf(&result, "Runtime: %s\n", time.Since(job.startTime).Round(time.Second))
	switch status {
	case statusCompleted, statusFailed:
		fmt.Fprintf(&result, "Exit Code: %d\n", exitCode)
	}
	result.WriteString("\n--- Output ---\n")
	if output == "" {
		result.WriteString("<no output>\n")
	} else {
		result.WriteString(output)
		if len(output) >= maxBackgroundJobOutputBytes {
			result.WriteString("\n\n[Output truncated at 10MB limit]")
		}
	}
	return result.String()
}

func (h *backgroundJobsHandler) ViewBackgroundJob(ctx context.Context, params ViewBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, exists := h.ownedJob(ctx, params.JobID)
	if !exists {
		return tools.ResultError("Job not found: " + params.JobID), nil
	}
	return tools.ResultSuccess(renderBackgroundJob(job)), nil
}

func (h *backgroundJobsHandler) ownedJob(ctx context.Context, jobID string) (*backgroundJob, bool) {
	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil {
		return nil, false
	}
	job, exists := h.jobs.Load(jobID)
	if !exists || job.owner != owner {
		return nil, false
	}
	return job, true
}

const defaultWaitTimeout = 60 * time.Second

func (h *backgroundJobsHandler) WaitBackgroundJob(ctx context.Context, params WaitBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, exists := h.ownedJob(ctx, params.JobID)
	if !exists {
		return tools.ResultError("Job not found: " + params.JobID), nil
	}
	return h.waitJob(ctx, job, params.Timeout)
}

func (h *backgroundJobsHandler) waitJob(ctx context.Context, job *backgroundJob, requestedTimeout int) (*tools.ToolCallResult, error) {
	timeout := defaultWaitTimeout
	if requestedTimeout > 0 {
		timeout = time.Duration(requestedTimeout) * time.Second
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-job.done:
		status := statusStrings[job.status.Load()]
		header := fmt.Sprintf("Job %s %s.\n\n", job.id, status)
		return tools.ResultSuccess(header + renderBackgroundJob(job)), nil
	case <-timer.C:
		header := fmt.Sprintf("Timed out after %s waiting for job %s; it is still running.\n\n",
			timeout.Round(time.Second), job.id)
		return tools.ResultSuccess(header + renderBackgroundJob(job)), nil
	case <-ctx.Done():
		return tools.ResultError(fmt.Sprintf("Wait cancelled for job %s: %s", job.id, ctx.Err())), nil
	}
}

func (h *backgroundJobsHandler) StopBackgroundJob(ctx context.Context, params StopBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, exists := h.ownedJob(ctx, params.JobID)
	if !exists {
		return tools.ResultError("Job not found: " + params.JobID), nil
	}

	if err := stopBackgroundJob(job); err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	return tools.ResultSuccess(fmt.Sprintf("Job %s stopped successfully", params.JobID)), nil
}

func stopBackgroundJob(job *backgroundJob) error {
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
			return fmt.Errorf("timed out waiting for job %s to stop", job.id)
		}
		return nil
	}
	terminate := terminateProcess
	if job.shellOwned {
		terminate = terminateShellProcessGroup
	}
	if err := terminate(job.process, job.processGroup, false); err != nil && !errors.Is(err, os.ErrProcessDone) {
		if job.status.Load() == statusRunning {
			return fmt.Errorf("error stopping job %s: %w", job.id, err)
		}
		return nil
	}
	job.stopRequested.Store(true)
	if job.work != nil {
		job.work.Cancel()
	}
	if !waitForJob(job.done, gracefulStopTimeout) {
		if err := terminate(job.process, job.processGroup, true); err != nil && !errors.Is(err, os.ErrProcessDone) && job.status.Load() == statusRunning {
			return fmt.Errorf("error force-stopping job %s: %w", job.id, err)
		}
		if !waitForJob(job.done, forcedStopTimeout) {
			return fmt.Errorf("timed out waiting for job %s to stop", job.id)
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

func formatBackgroundJobRecall(job *backgroundJob, status int32, exitCode int, output string) string {
	var result strings.Builder
	fmt.Fprintf(&result, "Background job %s finished with status %s (exit code %d).\n", job.id, statusToString(status), exitCode)
	fmt.Fprintf(&result, "Command: %s\n\n", job.cmd)
	result.WriteString("--- Output ---\n")
	if output == "" {
		result.WriteString("<no output>\n")
	} else {
		result.WriteString(output)
		if len(output) >= maxBackgroundJobOutputBytes {
			result.WriteString("\n\n[Output truncated at 10MB limit]")
		}
	}
	return builtins.BoundToolResult("background_jobs", ToolNameViewBackgroundJob, result.String(), "The retained output is available through view_background_job while this job exists.")
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

// CreateToolSet builds the background_jobs toolset from config.
func CreateToolSet(ctx context.Context, toolset latest.Toolset, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	env, err := toolsetEnv(ctx, toolset, runConfig)
	if err != nil {
		return nil, err
	}

	return CreateToolSetWithEnvironment(toolset, runConfig, env), nil
}

// ResolveEnvironment returns the exact process environment used by background jobs.
// Callers must not log or persist the returned values.
func ResolveEnvironment(ctx context.Context, toolset latest.Toolset, runConfig *config.RuntimeConfig) ([]string, error) {
	if runConfig == nil {
		return nil, errors.New("background_jobs requires runtime config")
	}
	return toolsetEnv(ctx, toolset, runConfig)
}

// CreateToolSetWithEnvironment uses a previously resolved effective environment,
// so a resource-pool key and the process receive identical inputs.
func CreateToolSetWithEnvironment(toolset latest.Toolset, runConfig *config.RuntimeConfig, effectiveEnv []string) *ToolSet {
	if runConfig == nil {
		runConfig = &config.RuntimeConfig{}
	}
	ts := New(slices.Clone(effectiveEnv), runConfig)
	if toolset.Recall != nil && *toolset.Recall {
		ts.handler.recall = true
	}
	return ts
}

func toolsetEnv(ctx context.Context, toolset latest.Toolset, runConfig *config.RuntimeConfig) ([]string, error) {
	env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), runConfig.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("failed to expand the toolset's environment variables: %w", err)
	}
	return append(os.Environ(), env...), nil
}

// New creates a background_jobs toolset with the supplied environment.
func New(env []string, runConfig *config.RuntimeConfig) *ToolSet {
	shell, argsPrefix := shellpath.DetectShell()

	handler := &backgroundJobsHandler{
		shell:           shell,
		shellArgsPrefix: argsPrefix,
		env:             env,
		jobs:            concurrent.NewMap[string, *backgroundJob](),
		workingDir:      runConfig.WorkingDir,
	}

	return &ToolSet{handler: handler}
}

func (h *backgroundJobsHandler) resolveWorkDir(cwd string) string {
	if cwd == "" || cwd == "." {
		return h.workingDir
	}
	if !filepath.IsAbs(cwd) {
		return filepath.Clean(filepath.Join(h.workingDir, cwd))
	}
	return cwd
}

func (t *ToolSet) Instructions() string {
	instructions := `## Background Job Tools

Use run_background_job for long-running processes (servers, watchers). Output capped at 10MB per job. Jobs belong to the originating session. Session cleanup stops the direct process and bounds the wait for output pipes. On Unix, child processes may survive; on Windows, cleanup terminates the job object. Daemonized or detached processes are not supported.

Use wait_background_job to block until a job finishes and retrieve its exit code and full output. Pass an optional timeout (seconds) to cap how long to wait; the job keeps running if the timeout fires.`
	if t.handler.recall {
		instructions += `

When starting a background job, set "recall": true if you want the agent loop to be steered automatically with a short completion message and the job output when it finishes.`
	}
	return instructions
}

func (t *ToolSet) runBackgroundJobDescription() string {
	description := `Starts a shell command in the background and returns immediately with a job ID. Use this for long-running processes like servers, watches, or any command that should run while other tasks are performed.`
	if t.handler.recall {
		description += ` Set recall=true to receive a steering message with the job output when it finishes.`
	}
	return description
}

func runBackgroundJobParameters(recall bool) any {
	if recall {
		return tools.MustSchemaFor[RunBackgroundJobRecallArgs]()
	}
	return tools.MustSchemaFor[RunBackgroundJobArgs]()
}

func (t *ToolSet) runBackgroundJobHandler() tools.ToolHandler {
	if t.handler.recall {
		return tools.NewRuntimeHandler(t.handler.RunBackgroundJobWithRecall)
	}
	return tools.NewRuntimeHandler(t.handler.RunBackgroundJob)
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:                    ToolNameRunBackgroundJob,
			Category:                "background_jobs",
			Description:             t.runBackgroundJobDescription(),
			Parameters:              runBackgroundJobParameters(t.handler.recall),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 t.runBackgroundJobHandler(),
			Annotations:             tools.ToolAnnotations{Title: "Background Job"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameListBackgroundJobs,
			Category:                "background_jobs",
			Description:             `Lists background jobs owned by this session with their status, runtime, and other information.`,
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.ListBackgroundJobs),
			Annotations:             tools.ToolAnnotations{Title: "List Background Jobs", ReadOnlyHint: true},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameViewBackgroundJob,
			Category:                "background_jobs",
			Description:             `Views the output and status of a specific background job by job ID.`,
			Parameters:              tools.MustSchemaFor[ViewBackgroundJobArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.ViewBackgroundJob),
			Annotations:             tools.ToolAnnotations{Title: "View Background Job Output", ReadOnlyHint: true},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameStopBackgroundJob,
			Category:                "background_jobs",
			Description:             `Stops a running background job owned by this session. Terminates the direct process on Unix (children may survive), or the job object on Windows, and waits a bounded time for captured output.`,
			Parameters:              tools.MustSchemaFor[StopBackgroundJobArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.StopBackgroundJob),
			Annotations:             tools.ToolAnnotations{Title: "Stop Background Job"},
			AddDescriptionParameter: true,
		},
		{
			Name:                    ToolNameWaitBackgroundJob,
			Category:                "background_jobs",
			Description:             `Blocks until a background job completes (or the optional timeout expires) and returns its exit code and captured output. Safe to call on an already-finished job — returns the cached result immediately. Use this instead of polling view_background_job in a loop.`,
			Parameters:              tools.MustSchemaFor[WaitBackgroundJobArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewHandler(t.handler.WaitBackgroundJob),
			Annotations:             tools.ToolAnnotations{Title: "Wait for Background Job", ReadOnlyHint: true},
			AddDescriptionParameter: true,
		},
	}, nil
}

func (h *backgroundJobsHandler) shellOwnedJob(ctx context.Context, id string) (*backgroundJob, bool) {
	job, ok := h.ownedJob(ctx, id)
	return job, ok && job.shellOwned
}
func (h *backgroundJobsHandler) ListShellBackgroundJobs(ctx context.Context, _ map[string]any) (*tools.ToolCallResult, error) {
	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil {
		return tools.ResultError("Error: background jobs require a session resource owner."), nil
	}
	var output strings.Builder
	output.WriteString("Shell Background Jobs:\n\n")
	count := 0
	h.jobs.Range(func(id string, job *backgroundJob) bool {
		if job.owner != owner || !job.shellOwned {
			return true
		}
		count++
		fmt.Fprintf(&output, "ID: %s\n  Command: %s\n  Status: %s\n", id, job.cmd, statusToString(job.status.Load()))
		return true
	})
	if count == 0 {
		output.WriteString("No shell background jobs found.\n")
	}
	return tools.ResultSuccess(output.String()), nil
}
func (h *backgroundJobsHandler) ViewShellBackgroundJob(ctx context.Context, args ViewBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, ok := h.shellOwnedJob(ctx, args.JobID)
	if !ok {
		return tools.ResultError("Job not found: " + args.JobID), nil
	}
	return tools.ResultSuccess(renderBackgroundJob(job)), nil
}
func (h *backgroundJobsHandler) WaitShellBackgroundJob(ctx context.Context, args WaitBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, ok := h.shellOwnedJob(ctx, args.JobID)
	if !ok {
		return tools.ResultError("Job not found: " + args.JobID), nil
	}
	return h.waitJob(ctx, job, args.Timeout)
}
func (h *backgroundJobsHandler) StopShellBackgroundJob(ctx context.Context, args StopBackgroundJobArgs) (*tools.ToolCallResult, error) {
	job, ok := h.shellOwnedJob(ctx, args.JobID)
	if !ok {
		return tools.ResultError("Job not found: " + args.JobID), nil
	}
	if err := stopBackgroundJob(job); err != nil {
		return tools.ResultError("Error: " + err.Error()), nil
	}
	return tools.ResultSuccess("Job " + args.JobID + " stopped successfully"), nil
}

func (t *ToolSet) Start(context.Context) error {
	return nil
}

// Close releases a raw standalone owner using a bounded cleanup context.
func (t *ToolSet) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return t.CloseAll(ctx)
}

// CloseAll is final raw-owner teardown after every environment lease is released.
// Session retirement must use StopResourceOwner so sibling session jobs survive.
func (t *ToolSet) CloseAll(ctx context.Context) error {
	var errs []error
	t.handler.jobs.Range(func(_ string, job *backgroundJob) bool {
		if job.status.Load() == statusRunning {
			if err := stopBackgroundJob(job); err != nil {
				errs = append(errs, err)
			}
		}
		select {
		case <-job.done:
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return true
		case <-time.After(forcedStopTimeout):
			errs = append(errs, errors.New("timed out waiting for background job cleanup"))
			return true
		}
		job.outputMu.Lock()
		job.rt = nil
		job.outputMu.Unlock()
		t.handler.jobs.Delete(job.id)
		return true
	})
	return errors.Join(errs...)
}

// JobCount reports retained jobs for lifecycle tests.
func (t *ToolSet) JobCount() int { return t.handler.jobs.Length() }

// SupportsShellBackground reports whether the host has a race-safe direct-child
// exit observer suitable for process-group cancellation escalation.
func (*ToolSet) SupportsShellBackground() bool { return shellBackgroundWaitSupported() }

func (t *ToolSet) StopResourceOwner(ctx context.Context) error {
	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil {
		return nil
	}
	retireErr := t.Stop(ctx)
	t.handler.jobs.Range(func(id string, job *backgroundJob) bool {
		if job.owner != owner {
			return true
		}
		timer := time.NewTimer(forcedStopTimeout)
		defer timer.Stop()
		select {
		case <-job.done:
			job.outputMu.Lock()
			job.rt = nil
			job.outputMu.Unlock()
			t.handler.jobs.Delete(id)
		case <-ctx.Done():
			retireErr = errors.Join(retireErr, ctx.Err())
		case <-timer.C:
			retireErr = errors.Join(retireErr, fmt.Errorf("timed out retiring job %s", id))
		}
		return true
	})
	return retireErr
}

func (t *ToolSet) Stop(ctx context.Context) error {
	owner := tools.ResourceOwnerFromContext(ctx)
	if owner == nil {
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error)
	t.handler.jobs.Range(func(_ string, job *backgroundJob) bool {
		if job.owner == owner && job.status.Load() == statusRunning {
			wg.Go(func() {
				if err := stopBackgroundJob(job); err != nil {
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
