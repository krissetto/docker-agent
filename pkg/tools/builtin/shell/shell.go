package shell

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/backgroundjobs"
)

const ToolNameShell = "shell"

// ToolSet provides synchronous shell command execution.
type ToolSet struct {
	handler             *shellHandler
	jobs                *backgroundjobs.ToolSet
	exposeJobInspectors bool
}

// Verify interface compliance
var (
	_ tools.ToolSet              = (*ToolSet)(nil)
	_ tools.Startable            = (*ToolSet)(nil)
	_ tools.Instructable         = (*ToolSet)(nil)
	_ tools.Elicitable           = (*ToolSet)(nil)
	_ tools.ResourceOwnerStopper = (*ToolSet)(nil)
)

type shellHandler struct {
	shell               string
	shellArgsPrefix     []string
	env                 []string
	timeout             time.Duration
	workingDir          string
	backgroundAfter     time.Duration
	jobs                *backgroundjobs.ToolSet
	exposeJobInspectors bool

	// sudoAskpass opts this toolset into the one-time sudo privilege
	// escalation flow (SUDO_ASKPASS bridged to the elicitation handler).
	sudoAskpass        bool
	elicitationMu      sync.RWMutex
	elicitationHandler tools.ElicitationHandler
	askpassMu          sync.Mutex
	askpassStarted     bool
	askpass            *askpassServer
}

type commandOutput struct {
	emit func(output string)
	mu   sync.Mutex
	buf  bytes.Buffer
}

// newCommandOutput adapts rt.EmitOutput to the ctx-less io.Writer contract
// of exec.Cmd; the closure scopes ctx to this command run.
func newCommandOutput(ctx context.Context, rt tools.Runtime) *commandOutput {
	return &commandOutput{emit: func(output string) {
		rt.EmitOutput(ctx, output)
	}}
}

func (o *commandOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	o.buf.Write(p) // bytes.Buffer.Write never errors
	o.mu.Unlock()

	if o.emit != nil {
		o.emit(string(p))
	}
	return len(p), nil
}

func (o *commandOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

type RunShellArgs struct {
	Cmd     string `json:"cmd" jsonschema:"Shell command"`
	Cwd     string `json:"cwd,omitempty" jsonschema:"Working directory (default \".\")"`
	Timeout int    `json:"timeout,omitempty" jsonschema:"Timeout in seconds (default 30)"`
}

// UnmarshalJSON accepts both the canonical "cmd" key and the common alias
// "command" for the shell command parameter.
//
// The advertised schema still declares "cmd" as the canonical name, but many
// models (particularly ones biased by Anthropic's built-in bash tool and other
// ecosystems that use "command") occasionally emit "command" instead. Accepting
// both prevents a wasted turn on an empty-command error while keeping the
// canonical contract unchanged.
//
// The command is resolved by [safety.CommandArg] over an exact-key map rather
// than by struct tags: encoding/json matches keys case-insensitively with
// last-wins, so {"cmd":"ls","CMD":"rm -rf x"} would run a command the runtime
// never classified. Sharing the resolver keeps the executed command identical
// to the labelled one.
func (a *RunShellArgs) UnmarshalJSON(data []byte) error {
	var raw struct {
		Cwd     string `json:"cwd"`
		Timeout int    `json:"timeout"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	a.Cmd, _ = safety.CommandArg(fields)
	a.Cwd = raw.Cwd
	a.Timeout = raw.Timeout
	return nil
}

func (h *shellHandler) RunShell(ctx context.Context, params RunShellArgs, rt tools.Runtime) (*tools.ToolCallResult, error) {
	if strings.TrimSpace(params.Cmd) == "" {
		return tools.ResultError(`Error: missing or empty "cmd" parameter. Pass the shell command as {"cmd": "..."}.`), nil
	}

	timeout := h.timeout
	if params.Timeout > 0 {
		timeout = time.Duration(params.Timeout) * time.Second
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cwd := h.resolveWorkDir(params.Cwd)

	// Stamp the call shape (cmd, cwd, timeout) onto the active span.
	// Cmd ships unconditionally — it's the main signal of what the
	// agent actually did, and gating it on chat-content capture loses
	// too much debug value. Drop or hash `cagent.tool.shell.cmd` at
	// the OTel collector if commands routinely carry secrets.
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(
			attribute.String("cagent.tool.shell.cmd", params.Cmd),
			attribute.Float64("cagent.tool.shell.timeout_seconds", timeout.Seconds()),
			attribute.String("cagent.tool.shell.cwd", cwd),
		)
	}

	slog.DebugContext(ctx, "Executing native shell command", "command", params.Cmd, "cwd", cwd)

	if msg := checkWorkDir(cwd); msg != "" {
		return tools.ResultError(msg), nil
	}

	return h.runNativeCommand(timeoutCtx, ctx, rt, params.Cmd, cwd, timeout), nil
}

// waitDelayAfterShellExit caps how long cmd.Wait() blocks on stdout/stderr
// copy goroutines after the direct shell child has exited.
//
// When cmd.Stdout/Stderr are not *os.File, Go's exec package creates OS pipes
// and spawns copy goroutines; cmd.Wait() only returns after *both* the child
// exits and those goroutines see EOF on the pipes. If the command backgrounds
// a grandchild (e.g. `docker run ... &`, `sleep 10 &`) that inherits the pipe
// fds, the pipes stay open and Wait() blocks until the configured timeout.
//
// cmd.WaitDelay tells Go to force-close the pipes and return this long after
// the direct child has exited, letting the grandchild keep running while the
// tool call returns promptly. A short delay is plenty because any output the
// shell itself produced is already flushed by the time it exits.
const waitDelayAfterShellExit = 500 * time.Millisecond

func (h *shellHandler) runNativeCommand(timeoutCtx, ctx context.Context, rt tools.Runtime, command, cwd string, timeout time.Duration) *tools.ToolCallResult {
	if h.jobs != nil && h.jobs.SupportsShellBackground() && h.backgroundAfter > 0 && !commandInvokesSudo(command) {
		owner := tools.ResourceOwnerFromContext(ctx)
		var work tools.BackgroundWork
		if bgRuntime, ok := rt.(tools.BackgroundWorkRuntime); ok && owner != nil {
			var err error
			work, err = bgRuntime.BeginBackgroundWork(ctx)
			if err != nil {
				return tools.ResultError(fmt.Sprintf("Error registering background shell command: %s", err))
			}
		}
		if work == nil {
			return h.runForegroundCommand(timeoutCtx, ctx, rt, command, cwd, timeout)
		}
		result := h.jobs.RunShellCommand(ctx, owner, h.shell, h.shellArgsPrefix, command, cwd, h.env, timeout, h.backgroundAfter, rt, work)
		if result.JobID != "" {
			return tools.ResultSuccess(fmt.Sprintf("Background job started with ID: %s\nCommand: %s\nWorking directory: %s\nInspect output with view_background_job.", result.JobID, command, cwd))
		}
		if result.TimedOut {
			return tools.ResultSuccess(formatCommandOutput(timeoutCtx, ctx, result.Err, result.Output, h.shell, timeout))
		}
		if ctx.Err() != nil {
			return tools.ResultSuccess(formatCommandOutput(timeoutCtx, ctx, result.Err, result.Output, h.shell, timeout))
		}
		return tools.ResultSuccess(formatCommandOutput(timeoutCtx, ctx, result.Err, result.Output, h.shell, timeout))
	}
	return h.runForegroundCommand(timeoutCtx, ctx, rt, command, cwd, timeout)
}

func (h *shellHandler) runForegroundCommand(timeoutCtx, ctx context.Context, rt tools.Runtime, command, cwd string, timeout time.Duration) *tools.ToolCallResult {
	var cmdEnv []string
	if scope, scoped := tools.HandlerScopeFrom(ctx); scoped && h.sudoAskpass && askpassSupported() && commandInvokesSudo(command) {
		srv, err := startAskpassServer(ctx, func() tools.ElicitationHandler { return scope.Elicitation })
		if err != nil {
			return tools.ResultError(fmt.Sprintf("Error starting sudo askpass helper: %s", err))
		}
		defer srv.close()
		command, cmdEnv = wrapSudoCommand(command, h.shell), append(append([]string(nil), h.env...), srv.env()...)
	} else {
		command, cmdEnv = h.applyAskpass(ctx, command)
	}
	cmd := exec.Command(h.shell, append(h.shellArgsPrefix, command)...)
	cmd.Env, cmd.Dir = cmdEnv, cwd
	cmd.SysProcAttr, cmd.WaitDelay = platformSpecificSysProcAttr(), waitDelayAfterShellExit
	output := newCommandOutput(ctx, rt)
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return tools.ResultError(fmt.Sprintf("Error starting command: %s", err))
	}
	pg, err := createProcessGroup(cmd.Process)
	if err != nil {
		reapSpawnedChild(cmd, pg)
		return tools.ResultError(fmt.Sprintf("Error creating process group: %s", err))
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var cmdErr error
	select {
	case <-timeoutCtx.Done():
		_ = kill(cmd.Process, pg)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	case cmdErr = <-done:
	}
	return tools.ResultSuccess(formatCommandOutput(timeoutCtx, ctx, cmdErr, output.String(), h.shell, timeout))
}

// reapSpawnedChild terminates a child that we've started but decided not
// to run (e.g. follow-up setup failed) and waits for it so we don't leak a
// zombie or its stdout/stderr pipes. SIGTERM is sent first; if the child
// hasn't exited after a short grace period we escalate to SIGKILL.
func reapSpawnedChild(cmd *exec.Cmd, pg *processGroup) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = kill(cmd.Process, pg)

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

// CreateToolSet is used by the tools registry.
func CreateToolSet(ctx context.Context, toolset latest.Toolset, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	env, err := toolsetEnv(ctx, toolset, runConfig)
	if err != nil {
		return nil, err
	}

	ts := New(env, runConfig)
	if toolset.BackgroundAfter != nil {
		ts.handler.backgroundAfter = time.Duration(*toolset.BackgroundAfter) * time.Second
	}
	if toolset.SudoAskpass != nil && *toolset.SudoAskpass {
		ts.handler.sudoAskpass = true
	}
	return ts, nil
}

func toolsetEnv(ctx context.Context, toolset latest.Toolset, runConfig *config.RuntimeConfig) ([]string, error) {
	env, err := environment.ExpandAll(ctx, environment.ToValues(toolset.Env), runConfig.EnvProvider())
	if err != nil {
		return nil, fmt.Errorf("failed to expand the toolset's environment variables: %w", err)
	}
	// Prepend os.Environ() so spawned processes inherit the host environment
	// while the configured toolset env still wins on key collisions
	// (exec.Cmd dedupes with last-wins). EnvProvider is used only to expand
	// ${...} references in toolset.Env.
	return append(os.Environ(), env...), nil
}

// New creates a new shell toolset.
func New(env []string, runConfig *config.RuntimeConfig) *ToolSet {
	shell, argsPrefix := detectShell()

	handler := &shellHandler{
		shell:           shell,
		shellArgsPrefix: argsPrefix,
		env:             env,
		timeout:         30 * time.Second,
		workingDir:      runConfig.WorkingDir,
		backgroundAfter: 2 * time.Second,
	}

	return &ToolSet{handler: handler}
}

// detectShell returns the appropriate shell and arguments based on the platform.
// It delegates to shellpath.DetectShell which uses absolute paths to prevent
// PATH hijacking (CWE-426).
func detectShell() (shell string, argsPrefix []string) {
	return shellpath.DetectShell()
}

// checkWorkDir verifies the working directory exists and is a directory,
// returning a user-facing error message (empty when OK). Without this check,
// a missing cwd surfaces as the cryptic "fork/exec <shell>: no such file or
// directory": the child's chdir failure is misattributed to the shell binary
// when SysProcAttr forces the raw fork+exec path. Best-effort: the directory
// can still disappear before exec; this only improves the common-case message.
func checkWorkDir(cwd string) string {
	if cwd == "" {
		return "" // empty Dir means "inherit the process cwd", always valid
	}
	info, err := os.Stat(cwd)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "Error: working directory does not exist: " + cwd
	case err != nil:
		return fmt.Sprintf("Error: cannot access working directory %s: %s", cwd, err)
	case !info.IsDir():
		return "Error: working directory is not a directory: " + cwd
	}
	return ""
}

// resolveWorkDir returns the effective working directory.
func (h *shellHandler) resolveWorkDir(cwd string) string {
	if cwd == "" || cwd == "." {
		return h.workingDir
	}
	if !filepath.IsAbs(cwd) {
		return filepath.Clean(filepath.Join(h.workingDir, cwd))
	}
	return cwd
}

// formatCommandOutput formats command output handling timeout, cancellation, and errors.
// shellPath gates shellDialectHint: a raw substring match would false-positive on
// macOS grep or docker logs from a Windows container.
func formatCommandOutput(timeoutCtx, ctx context.Context, err error, rawOutput, shellPath string, timeout time.Duration) string {
	var output string
	if timeoutCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		if ctx.Err() != nil {
			output = "Command cancelled"
		} else {
			output = fmt.Sprintf("Command timed out after %v\nOutput: %s", timeout, rawOutput)
		}
	} else {
		output = rawOutput
		if err != nil {
			output = fmt.Sprintf("Error executing command: %s\nOutput: %s", err, output)
		}
	}
	output = cmp.Or(strings.TrimSpace(output), "<no output>")
	if hint := shellDialectHint(shellpath.ShellBaseName(shellPath), output); hint != "" {
		output = "[shell-hint] " + hint + "\n\n" + output
	}
	return output
}

// shellDialectHint returns a corrective hint for known dialect errors. Gates on
// shell because e.g. a "chain with &&" nudge is wrong when PowerShell is the
// parent and a child cmd.exe fails.
func shellDialectHint(shell, output string) string {
	switch shell {
	case "powershell":
		switch {
		case strings.Contains(output, "'&&' is not a valid statement separator"),
			strings.Contains(output, "'||' is not a valid statement separator"):
			return "You are on Windows PowerShell 5.1: chain commands with `;`, not `&&` or `||`. Retry with the corrected syntax."
		case strings.Contains(output, `'C:\dev\null'`):
			return "You are on Windows: redirect stderr with `2>$null` (PowerShell) or `2>nul` (cmd.exe), NOT `2>/dev/null`."
		case strings.Contains(output, "is not recognized as the name of a cmdlet"):
			return "You are on PowerShell: POSIX utilities are not available. Use `Select-String` (grep), `Select-Object -First N` (head), `Get-ChildItem` (ls), `Get-Content` (cat), `Test-Path` (test -f)."
		case strings.Contains(output, "A parameter cannot be found that matches parameter name"):
			return "You are on PowerShell: cmdlets do not accept POSIX-style short flags like `-la`. Use the cmdlet's own parameter names (e.g. `Get-ChildItem -Force -Recurse`, `Select-Object -First N`)."
		}
	case "pwsh":
		switch {
		case strings.Contains(output, `'C:\dev\null'`):
			return "You are on Windows: redirect stderr with `2>$null` (PowerShell) or `2>nul` (cmd.exe), NOT `2>/dev/null`."
		case strings.Contains(output, "is not recognized as a name of a cmdlet"):
			return "You are on PowerShell: POSIX utilities are not available. Use `Select-String` (grep), `Select-Object -First N` (head), `Get-ChildItem` (ls), `Get-Content` (cat), `Test-Path` (test -f)."
		case strings.Contains(output, "A parameter cannot be found that matches parameter name"):
			return "You are on PowerShell: cmdlets do not accept POSIX-style short flags like `-la`. Use the cmdlet's own parameter names (e.g. `Get-ChildItem -Force -Recurse`, `Select-Object -First N`)."
		}
	case "cmd":
		if strings.Contains(output, "is not recognized as an internal or external command") {
			return "You are on cmd.exe: POSIX utilities are not available. Use `findstr` (grep), `type` (cat), `dir` (ls), and chain commands with `&` or `&&`."
		}
	}
	return ""
}

func (t *ToolSet) Instructions() string {
	return fmt.Sprintf(`## Shell Tools

- Commands are executed by %s on %s — write them in that shell's syntax
- Each call runs in a fresh shell session — no state persists between calls
- Default timeout: 30s. Set "timeout" for longer operations (builds, tests)
%s
- Use "cwd" parameter instead of cd within commands
- Combine operations with pipes, redirections, and heredocs
- Non-zero exit codes return error info with output; timed-out commands are terminated`,
		shellpath.ShellBaseName(t.handler.shell), displayOS(), t.handler.backgroundInstructions())
}

func (h *shellHandler) backgroundInstructions() string {
	if h.jobs == nil || !h.jobs.SupportsShellBackground() || h.backgroundAfter <= 0 {
		return "Automatic shell backgrounding is unavailable in this host or disabled."
	}
	return fmt.Sprintf("Commands still running after %s continue as background jobs; inspect with view_background_job.", h.backgroundAfter)
}

func (t *ToolSet) AttachBackgroundJobs(jobs *backgroundjobs.ToolSet, exposeInspectors bool) {
	t.jobs, t.handler.jobs, t.exposeJobInspectors = jobs, jobs, exposeInspectors
}

func (t *ToolSet) Tools(ctx context.Context) ([]tools.Tool, error) {
	result := []tools.Tool{
		{
			Name:                    ToolNameShell,
			Category:                "shell",
			Description:             fmt.Sprintf("%s %s", shellToolDescription(t.handler.shell), t.handler.backgroundInstructions()),
			Parameters:              tools.MustSchemaFor[RunShellArgs](),
			OutputSchema:            tools.MustSchemaFor[string](),
			Handler:                 tools.NewRuntimeHandler(t.handler.RunShell),
			Annotations:             tools.ToolAnnotations{Title: "Shell"},
			AddDescriptionParameter: true,
		},
	}
	if t.jobs != nil && t.exposeJobInspectors {
		inspectors, err := t.jobs.ShellInspectionTools()
		if err != nil {
			return nil, err
		}
		result = append(result, inspectors...)
	}
	return result, nil
}

// SetElicitationHandler wires the runtime's elicitation handler into the shell
// toolset. It is used by the sudo askpass flow to prompt the user for their
// password. The handler is re-applied at the start of every turn, so this must
// stay idempotent.
func (t *ToolSet) SetElicitationHandler(handler tools.ElicitationHandler) {
	t.handler.setElicitationHandler(handler)
}

func (t *ToolSet) Start(context.Context) error {
	return nil
}

func (t *ToolSet) Stop(context.Context) error {
	t.handler.stopAskpass()
	return nil
}

func (t *ToolSet) StopResourceOwner(ctx context.Context) error {
	if t.jobs == nil {
		return nil
	}
	return t.jobs.StopResourceOwner(ctx)
}

// shellToolDescription names the interpreter that will run the command.
// Models default to POSIX syntax when the description only says "the
// user's default shell", which wastes turns on Windows where the
// resolved shell is PowerShell or cmd.exe (e.g. "pwd && ls -la" is a
// parse error under Windows PowerShell 5.1). The trailing nudge points
// the model at get_environment_info for edge cases the static hint
// does not cover (custom shells, multi-agent handoffs, etc.).
func shellToolDescription(shellPath string) string {
	name := shellpath.ShellBaseName(shellPath)
	desc := fmt.Sprintf("Executes the given shell command with %s on %s.", name, displayOS())
	if hint := shellSyntaxHint(name); hint != "" {
		desc += " " + hint
	}
	desc += " If unsure which shell syntax to use, call get_environment_info first."
	return desc
}

// shellSyntaxHint returns a dialect warning for shells that models
// commonly mistake for a POSIX shell. Empty for shells where the name
// alone is enough of a cue. The substitution list names the utilities
// that actually show up in observed failure traces, not a hypothetical
// full mapping — 'grep'/'head'/'tail'/'wc'/'cat'/'gunzip' plus 'ls -la'
// and '&&' cover the dominant error signatures.
func shellSyntaxHint(name string) string {
	switch name {
	case "powershell":
		// Windows PowerShell: '&&' / '||' are parse errors before v7,
		// and POSIX utilities are not available. Version is not
		// asserted here — the binary name alone can't tell us which.
		return `Windows PowerShell dialect. Chain commands with ";" (not "&&"; "&&" is a parse error on 5.1). POSIX utilities are not available — use Select-String (not grep), Select-Object -First N (not head), -Last N (not tail), Measure-Object -Line (not wc -l), Get-Content (not cat), Expand-Archive (not gunzip/tar). "ls -la" fails — use "ls" or "Get-ChildItem".`
	case "pwsh":
		// PowerShell 7+: '&&' / '||' work, but POSIX utilities still
		// don't exist as cmdlets.
		return `PowerShell 7+ dialect. POSIX utilities are not available as cmdlets — use Select-String (not grep), Select-Object -First N (not head), -Last N (not tail), Measure-Object -Line (not wc -l), Get-Content (not cat), Expand-Archive (not gunzip/tar). "ls -la" fails — use "ls" or "Get-ChildItem".`
	case "cmd":
		return `cmd.exe syntax, not POSIX. No POSIX utilities (grep, head, tail, wc, cat), no POSIX flags on built-ins, and variable expansion uses %VAR% (not $VAR).`
	default:
		return ""
	}
}

// displayOS returns a friendlier label for the common values of
// runtime.GOOS, falling back to GOOS itself for anything exotic.
func displayOS() string {
	switch goruntime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	default:
		return goruntime.GOOS
	}
}
