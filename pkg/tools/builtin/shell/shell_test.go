package shell

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/safety"
	"github.com/docker/docker-agent/pkg/shellpath"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/shellprocess"
)

func TestNew(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	assert.NotNil(t, tool)
	assert.NotNil(t, tool.handler)
	assertPlatformDefaultShell(t, tool.handler, "/bin/bash")

	t.Setenv("SHELL", "")
	tool = New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	assert.NotNil(t, tool)
	assert.NotNil(t, tool.handler)
	assertPlatformDefaultShell(t, tool.handler, "/bin/sh")
}

func TestShellTool_HandlerEcho(t *testing.T) {
	t.Parallel()
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{
		Cmd: "echo 'hello world'",
		Cwd: "",
	}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "hello world")
}

func TestShellTool_HandlerWithCwd(t *testing.T) {
	t.Parallel()
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	tmpDir := t.TempDir()

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{
		Cmd: pwdCmd(),
		Cwd: tmpDir,
	}, tools.NopRuntime{})
	require.NoError(t, err)
	assertSamePath(t, tmpDir, strings.TrimSpace(result.Output))
}

func TestRunShellArgs_UnmarshalJSON_AcceptsCmdAndCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantCmd string
		wantCwd string
		wantTO  int
	}{
		{
			name:    "canonical cmd",
			input:   `{"cmd":"ls -la","cwd":"/tmp","timeout":10}`,
			wantCmd: "ls -la",
			wantCwd: "/tmp",
			wantTO:  10,
		},
		{
			name:    "alias command",
			input:   `{"command":"ls -la","cwd":"/tmp","timeout":10}`,
			wantCmd: "ls -la",
			wantCwd: "/tmp",
			wantTO:  10,
		},
		{
			name:    "both present cmd wins",
			input:   `{"cmd":"from-cmd","command":"from-command"}`,
			wantCmd: "from-cmd",
		},
		{
			name:    "blank cmd falls back to command alias",
			input:   `{"cmd":"   ","command":"from-command"}`,
			wantCmd: "from-command",
		},
		{
			name:    "empty cmd falls back to command alias",
			input:   `{"cmd":"","command":"from-command"}`,
			wantCmd: "from-command",
		},
		{
			name:    "empty object leaves cmd empty",
			input:   `{}`,
			wantCmd: "",
		},
		{
			// encoding/json struct decoding would let a mixed-case key
			// override the exact one the runtime classified.
			name:    "mixed-case keys are not aliases",
			input:   `{"cmd":"git status","CMD":"rm -rf /tmp/x","Command":"rm -rf /"}`,
			wantCmd: "git status",
		},
		{
			name:    "non-string cmd is ignored, alias runs",
			input:   `{"cmd":42,"command":"from-command"}`,
			wantCmd: "from-command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got RunShellArgs
			require.NoError(t, json.Unmarshal([]byte(tt.input), &got))
			assert.Equal(t, tt.wantCmd, got.Cmd)
			assert.Equal(t, tt.wantCwd, got.Cwd)
			assert.Equal(t, tt.wantTO, got.Timeout)

			// The executed command must be exactly what the runtime labels.
			var fields map[string]any
			require.NoError(t, json.Unmarshal([]byte(tt.input), &fields))
			classified, _ := safety.CommandArg(fields)
			assert.Equal(t, classified, got.Cmd)
		})
	}
}

// Exercises the end-to-end dispatch path: a tool-call whose raw arguments
// use "command" instead of "cmd" must execute normally rather than return
// the missing-parameter error.
func TestShellTool_HandlerAcceptsCommandAlias(t *testing.T) {
	t.Parallel()
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	var params RunShellArgs
	require.NoError(t, json.Unmarshal([]byte(`{"command":"echo hello-from-alias"}`), &params))

	result, err := tool.handler.RunShell(t.Context(), params, tools.NopRuntime{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "hello-from-alias")
}

func TestShellTool_HandlerMissingCmdReturnsActionableError(t *testing.T) {
	t.Parallel()
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.Contains(t, result.Output, `"cmd"`,
		"error must name the expected parameter so the model can self-correct")
}

func TestShellTool_HandlerError(t *testing.T) {
	t.Parallel()
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{
		Cmd: "command_that_does_not_exist",
		Cwd: "",
	}, tools.NopRuntime{})
	require.NoError(t, err, "Handler should not return an error")
	assert.Contains(t, result.Output, "Error executing command")
}

func TestShellTool_OutputSchema(t *testing.T) {
	t.Parallel()
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		assert.NotNil(t, tool.OutputSchema)
	}
}

func TestShellTool_ParametersAreObjects(t *testing.T) {
	t.Parallel()
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, allTools)

	for _, tool := range allTools {
		m, err := tools.SchemaToMap(tool.Parameters)
		require.NoError(t, err)
		assert.Equal(t, "object", m["type"])
	}
}

func TestShellTool_OnlyExposesShell(t *testing.T) {
	t.Parallel()
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	assert.Equal(t, ToolNameShell, allTools[0].Name)
}

func TestShellTool_Instructions(t *testing.T) {
	t.Parallel()

	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	processes := shellprocess.New()
	tool.AttachProcessManager(processes, true)
	t.Cleanup(func() { require.NoError(t, processes.CloseAll(t.Context())) })

	instructions := tool.Instructions()

	assert.Contains(t, instructions, "Shell Tools")
	assert.Contains(t, instructions, shellpath.ShellBaseName(tool.handler.shell),
		"instructions must name the resolved shell so the model uses its syntax")
	assert.Contains(t, instructions, displayOS())
	if processes.SupportsShellBackground() {
		assertShellBackgroundGuidance(t, instructions)
	}
}

func assertShellBackgroundGuidance(t *testing.T, text string) {
	t.Helper()
	for _, phrase := range []string{
		"wakes/resumes this agent",
		"do independent work",
		"end your turn without tool calls",
		"no keep-alive needed",
		"Never poll",
		"inspect_running_process",
		"list_running_processes",
		"sleep",
		"dummy calls",
		"concrete diagnostic or user request",
		"intentionally cancel",
	} {
		assert.Contains(t, text, phrase)
	}
}

// TestShellTool_DescriptionNamesInterpreter guards against regressing to a
// generic "the user's default shell" description: without the resolved shell
// name, models assume POSIX and emit commands that fail under PowerShell/cmd.
func TestShellTool_DescriptionNamesInterpreter(t *testing.T) {
	t.Parallel()

	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	processes := shellprocess.New()
	tool.AttachProcessManager(processes, false)
	t.Cleanup(func() { require.NoError(t, processes.CloseAll(t.Context())) })

	allTools, err := tool.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)

	description := allTools[0].Description
	assert.Contains(t, description, shellpath.ShellBaseName(tool.handler.shell))
	assert.Contains(t, description, displayOS())
	assert.Contains(t, description, "get_environment_info",
		"description must point the model at the environment tool for edge cases the static hint doesn't cover")
	if processes.SupportsShellBackground() {
		assertShellBackgroundGuidance(t, description)
	}
}

func TestShellDialectHint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		shell    string
		output   string
		wantHint string // substring the hint must contain; empty means no hint
	}{
		{
			name:     "powershell 5.1 && parse error",
			shell:    "powershell",
			output:   "At line:1 char:17\n+ docker ps && docker images\n+                 ~~\nThe token '&&' is not a valid statement separator in this version.",
			wantHint: "chain commands with `;`",
		},
		{
			name:     "powershell 5.1 || parse error",
			shell:    "powershell",
			output:   "The token '||' is not a valid statement separator in this version.",
			wantHint: "chain commands with `;`",
		},
		{
			name:     "powershell posix stderr redirection",
			shell:    "powershell",
			output:   `out-file : Could not find a part of the path 'C:\dev\null'.`,
			wantHint: "2>$null",
		},
		{
			name:     "powershell 5.1 grep not a cmdlet",
			shell:    "powershell",
			output:   "grep : The term 'grep' is not recognized as the name of a cmdlet, function, script file, or operable program.",
			wantHint: "Select-String",
		},
		{
			name:     "pwsh 7 head not a cmdlet",
			shell:    "pwsh",
			output:   "head : The term 'head' is not recognized as a name of a cmdlet, function, script file, or executable program.",
			wantHint: "Select-Object -First",
		},
		{
			name:     "cmd.exe unknown command",
			shell:    "cmd",
			output:   "'grep' is not recognized as an internal or external command,\noperable program or batch file.",
			wantHint: "findstr",
		},
		{
			name:     "powershell rejects posix short flag",
			shell:    "powershell",
			output:   "Get-ChildItem : A parameter cannot be found that matches parameter name 'la'.",
			wantHint: "POSIX-style short flags",
		},
		{
			name:     "zsh does not fire windows hints",
			shell:    "zsh",
			output:   "The token '&&' is not a valid statement separator in this version.",
			wantHint: "",
		},
		{
			name:     "powershell does not fire cmd.exe hint on child cmd failure",
			shell:    "powershell",
			output:   "'grep' is not recognized as an internal or external command,\noperable program or batch file.",
			wantHint: "",
		},
		{
			name:     "pwsh 7 does not fire powershell-5.1-only && hint",
			shell:    "pwsh",
			output:   "The token '&&' is not a valid statement separator in this version.",
			wantHint: "",
		},
		{
			name:     "benign output leaves no hint",
			shell:    "powershell",
			output:   "hello world",
			wantHint: "",
		},
		{
			name:     "docker error unrelated to shell dialect",
			shell:    "powershell",
			output:   "Error response from daemon: No such container: comfy-worker",
			wantHint: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shellDialectHint(tt.shell, tt.output)
			if tt.wantHint == "" {
				assert.Empty(t, got, "expected no hint for shell=%q output=%q", tt.shell, tt.output)
				return
			}
			require.NotEmpty(t, got, "expected a hint for shell=%q output=%q", tt.shell, tt.output)
			assert.Contains(t, got, tt.wantHint,
				"hint for shell=%q output=%q missing %q; got %q", tt.shell, tt.output, tt.wantHint, got)
		})
	}
}

// TestShellSyntaxHint_pwshDoesNotClaimAmpAmpFails guards against copy-pasting
// the powershell (5.1) `&&`-is-a-parse-error clause into the pwsh (7+) hint:
// pipeline-chain operators shipped in PowerShell 7, and telling a pwsh session
// to avoid them is actively wrong.
func TestShellSyntaxHint_pwshDoesNotClaimAmpAmpFails(t *testing.T) {
	t.Parallel()
	assert.NotContains(t, shellSyntaxHint("pwsh"), "&&")
}

func TestFormatCommandOutput_PrependsHint(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	raw := "The token '&&' is not a valid statement separator in this version."
	got := formatCommandOutput(timeoutCtx, ctx, nil, raw, `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, time.Minute)

	assert.True(t, strings.HasPrefix(got, "[shell-hint] "),
		"hint must lead the output so the model sees it first; got %q", got)
	assert.Contains(t, got, raw, "original output must be preserved after the hint")
}

// TestFormatCommandOutput_NoHintOnPosixShell: raw output mentioning a Windows
// error string on a POSIX host must not trigger the hint.
func TestFormatCommandOutput_NoHintOnPosixShell(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	raw := "The token '&&' is not a valid statement separator in this version."
	got := formatCommandOutput(timeoutCtx, ctx, nil, raw, "/bin/zsh", time.Minute)
	assert.Equal(t, raw, got)
}

func TestFormatCommandOutput_NoHintOnBenignOutput(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	got := formatCommandOutput(timeoutCtx, ctx, nil, "hello world", `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, time.Minute)
	assert.Equal(t, "hello world", got)
}

func TestResolveWorkDir(t *testing.T) {
	t.Parallel()

	// resolveWorkDir is pure path manipulation; the directories need not
	// exist. Deriving them from TempDir keeps the expectations portable
	// (drive-letter absolutes on Windows, slash-rooted elsewhere).
	workingDir := filepath.Join(t.TempDir(), "configured", "project")
	other := filepath.Join(t.TempDir(), "other")
	h := &shellHandler{workingDir: workingDir}

	tests := []struct {
		name     string
		cwd      string
		expected string
	}{
		{name: "empty defaults to workingDir", cwd: "", expected: workingDir},
		{name: "dot defaults to workingDir", cwd: ".", expected: workingDir},
		{name: "absolute path unchanged", cwd: other, expected: other},
		{name: "relative path joined with workingDir", cwd: "src/pkg", expected: filepath.Join(workingDir, "src", "pkg")},
		{name: "relative with dot prefix", cwd: "./subdir", expected: filepath.Join(workingDir, "subdir")},
		{name: "relative with parent traversal", cwd: "../sibling", expected: filepath.Join(filepath.Dir(workingDir), "sibling")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, h.resolveWorkDir(tt.cwd))
		})
	}
}

func TestCheckWorkDir(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, nil, 0o644))

	assert.Empty(t, checkWorkDir(""), "empty cwd inherits the process cwd")
	assert.Empty(t, checkWorkDir(t.TempDir()))
	assert.Contains(t, checkWorkDir(filepath.Join(t.TempDir(), "gone")), "does not exist")
	assert.Contains(t, checkWorkDir(file), "not a directory")
}

// Regression test: a session working directory that no longer exists (e.g. a
// removed git worktree restored for a new tab) must yield a clear error, not
// the misleading "fork/exec <shell>: no such file or directory" produced by
// the child's chdir failure.
func TestShellTool_MissingWorkingDir(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "gone")
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: missing}})

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{Cmd: "pwd"}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Output, "working directory does not exist")
	assert.Contains(t, result.Output, missing)
	assert.NotContains(t, result.Output, "fork/exec")
}

func TestShellTool_RelativeCwdResolvesAgainstWorkingDir(t *testing.T) {
	t.Parallel()
	// Create a directory structure: workingDir/subdir/
	workingDir := t.TempDir()
	subdir := filepath.Join(workingDir, "subdir")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: workingDir}})

	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{
		Cmd: pwdCmd(),
		Cwd: "subdir",
	}, tools.NopRuntime{})
	require.NoError(t, err)
	// The relative cwd must resolve against the configured workingDir,
	// not the process cwd.
	assertSamePath(t, subdir, strings.TrimSpace(result.Output))
}

// Regression test for a shell-tool hang caused by backgrounded grandchildren.
//
// A command like `sleep 10 &` makes the shell exit immediately, but the
// backgrounded sleep inherits stdout/stderr. Without cmd.WaitDelay, Go's
// exec.Cmd.Wait() blocks reading the pipe until the configured timeout,
// which makes the tool call hang (observed in eval runs where the agent
// launched a server with `docker run ... &`).
//
// With the WaitDelay safeguard the tool must return within a small fraction
// of the configured timeout.
func TestShellTool_BackgroundedChildDoesNotBlockReturn(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell backgrounding semantics; skipped on Windows")
	}

	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	start := time.Now()
	result, err := tool.handler.RunShell(t.Context(), RunShellArgs{
		// sleep inherits stdout/stderr from the shell and holds the pipe
		// open for 30s. The tool must return as soon as the shell exits.
		Cmd:     "sleep 30 &",
		Timeout: 20,
	}, tools.NopRuntime{})
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Less(t, elapsed, 5*time.Second,
		"shell tool must return promptly when the command backgrounds a child "+
			"that inherits stdout/stderr; elapsed=%s", elapsed)
}

// Even when the backgrounded child detaches into its own session (so the
// shell tool's process-group kill cannot reach it on timeout), cmd.WaitDelay
// must still allow the tool call to return.
func TestShellTool_DetachedBackgroundedChildDoesNotBlockReturn(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell backgrounding semantics; skipped on Windows")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not available")
	}

	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})

	done := make(chan struct{})
	var result *tools.ToolCallResult
	var err error
	go func() {
		defer close(done)
		result, err = tool.handler.RunShell(t.Context(), RunShellArgs{
			// setsid places sleep in its own session/process group, so the
			// process-group kill fallback in the timeout path cannot reach
			// it. Only cmd.WaitDelay can unblock Wait() here.
			Cmd:     "setsid sleep 30 &",
			Timeout: 20,
		}, tools.NopRuntime{})
	}()

	select {
	case <-done:
		require.NoError(t, err)
		require.NotNil(t, result)
	case <-time.After(10 * time.Second):
		t.Fatal("shell tool hung when command backgrounded a detached child")
	}
}

// TestReapSpawnedChild verifies that reapSpawnedChild both terminates a
// running child and waits on it so no zombie is left behind. This exercises
// the error path we take when cmd.Start() succeeded but a follow-up call
// (e.g. createProcessGroup) failed.
func TestReapSpawnedChild(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-specific: relies on /bin/sh and ProcessState.Exited()")
	}

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 60")
	cmd.SysProcAttr = platformSpecificSysProcAttr()
	require.NoError(t, cmd.Start())

	start := time.Now()
	reapSpawnedChild(cmd, nil)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 3*time.Second, "reapSpawnedChild should return promptly after kill")
	require.NotNil(t, cmd.ProcessState, "ProcessState must be populated - Wait() was not called")
	// After Wait(), ProcessState.Exited() returns false for signaled
	// processes but the important property is that the child was reaped,
	// which is exactly what ProcessState != nil guarantees.
}

// TestReapSpawnedChild_HandlesAlreadyExited verifies that reaping a process
// that has already exited is a no-op (does not block, does not panic).
func TestReapSpawnedChild_HandlesAlreadyExited(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX-specific")
	}

	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 0")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	// EOF on stdout means the child has exited (its fds are closed on exit).
	_, err = io.ReadAll(stdout)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		reapSpawnedChild(cmd, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reapSpawnedChild hung on an already-exited process")
	}
	require.NotNil(t, cmd.ProcessState, "process must have been reaped")
}

func TestShellAutoBackgroundsSameProcessAndInspectors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process test")
	}
	jobs := shellprocess.New()
	shellTool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	shellTool.AttachProcessManager(jobs, true)
	shellTool.handler.backgroundAfter = 30 * time.Millisecond
	owner := tools.NewResourceOwner()
	work := &testBackgroundWork{complete: make(chan string, 1), closed: make(chan struct{})}
	rt := &backgroundWorkRuntime{work: work}
	ctx := tools.WithResourceOwner(t.Context(), owner)
	marker := filepath.Join(t.TempDir(), "runs")
	result, err := shellTool.handler.RunShell(ctx, RunShellArgs{Cmd: "echo start >> " + marker + "; sleep 0.2; echo after-handoff", Timeout: 2}, rt)
	require.NoError(t, err)
	require.Contains(t, result.Output, "Completion output will be delivered automatically.")
	parsedID, parsed := tools.ParseShellBackgroundHandoff(result.Output)
	require.True(t, parsed)
	require.NotEmpty(t, parsedID)
	assertShellBackgroundGuidance(t, result.Output)
	id := strings.Fields(strings.Split(result.Output, "process ID ")[1])[0]
	inspected, err := shellTool.Tools(t.Context())
	require.NoError(t, err)
	var view *tools.ToolCallResult
	for _, candidate := range inspected {
		if candidate.Name == shellprocess.ToolNameInspectRunningProcess {
			view, err = candidate.Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: candidate.Name, Arguments: `{"process_id":"` + id + `"}`}}, tools.NopRuntime{})
			require.NoError(t, err)
		}
	}
	require.NotNil(t, view)
	require.Eventually(t, func() bool { _, err := os.Stat(marker); return err == nil }, time.Second, time.Millisecond)
	markerBytes, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "start\n", string(markerBytes), "background handoff must continue the same process, not rerun the command")
	select {
	case message := <-work.complete:
		require.Contains(t, message, "after-handoff")
		completion, parsed := tools.ParseShellCompletion(message)
		require.True(t, parsed)
		require.Equal(t, parsedID, completion.ProcessID)
	case <-time.After(3 * time.Second):
		t.Fatal("completion was not delivered")
	}
	toolsList, err := shellTool.Tools(t.Context())
	require.NoError(t, err)
	names := make([]string, 0, len(toolsList))
	for _, tool := range toolsList {
		names = append(names, tool.Name)
	}
	require.Contains(t, names, shellprocess.ToolNameListRunningProcesses)
	require.Contains(t, names, shellprocess.ToolNameInspectRunningProcess)
	require.Contains(t, names, shellprocess.ToolNameKillRunningProcess)
	inspected, err = shellTool.Tools(t.Context())
	require.NoError(t, err)
	var inspectAfter *tools.ToolCallResult
	for _, candidate := range inspected {
		if candidate.Name == shellprocess.ToolNameInspectRunningProcess {
			inspectAfter, err = candidate.Handler(ctx, tools.ToolCall{Function: tools.FunctionCall{Name: candidate.Name, Arguments: `{"process_id":"` + id + `"}`}}, tools.NopRuntime{})
			require.NoError(t, err)
		}
	}
	require.NotNil(t, inspectAfter)
	require.True(t, inspectAfter.IsError, "completed output is delivered once, not retained for manual inspection")
	select {
	case <-work.closed:
	case <-time.After(time.Second):
		t.Fatal("background work was not closed")
	}
	require.NoError(t, jobs.StopResourceOwner(ctx))
}

func TestShellForegroundFastAndDisabledBackgrounding(t *testing.T) {
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	jobs := shellprocess.New()
	tool.AttachProcessManager(jobs, false)
	tool.handler.backgroundAfter = 500 * time.Millisecond
	ctx := tools.WithResourceOwner(t.Context(), tools.NewResourceOwner())
	rt := &backgroundWorkRuntime{work: &testBackgroundWork{complete: make(chan string, 1), closed: make(chan struct{})}}
	fast, err := tool.handler.RunShell(ctx, RunShellArgs{Cmd: "echo fast"}, rt)
	require.NoError(t, err)
	require.Contains(t, fast.Output, "fast")
	require.NotContains(t, fast.Output, "Background job started")
	require.Zero(t, jobs.ProcessCount())
	tool.handler.backgroundAfter = 0
	slow, err := tool.handler.RunShell(ctx, RunShellArgs{Cmd: "sleep 0.6; echo disabled"}, rt)
	require.NoError(t, err)
	require.Contains(t, slow.Output, "disabled")
	require.NotContains(t, slow.Output, "Background job started")
	require.Zero(t, jobs.ProcessCount())
}

type testBackgroundWork struct {
	complete chan string
	closed   chan struct{}
	once     sync.Once
}

func (w *testBackgroundWork) Complete(message string) error { w.complete <- message; return nil }
func (w *testBackgroundWork) Cancel()                       {}
func (w *testBackgroundWork) Close() error                  { w.once.Do(func() { close(w.closed) }); return nil }

type backgroundWorkRuntime struct {
	tools.NopRuntime
	work tools.BackgroundWork
}

func (r *backgroundWorkRuntime) BeginBackgroundWork(context.Context) (tools.BackgroundWork, error) {
	return r.work, nil
}
func (*backgroundWorkRuntime) Supports(capability tools.Capability) bool {
	return capability == tools.CapabilityBackgroundWork
}

func TestShellBackgroundAfterDefaultsAndCanBeDisabled(t *testing.T) {
	defaultTool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	require.Equal(t, 2*time.Second, defaultTool.handler.backgroundAfter)
	zero := 0
	configured, err := CreateToolSet(t.Context(), latest.Toolset{Type: "shell", BackgroundAfter: &zero}, &config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}})
	require.NoError(t, err)
	raw, ok := tools.As[*ToolSet](configured)
	require.True(t, ok)
	require.Zero(t, raw.handler.backgroundAfter)
}

func TestShellWithoutBackgroundWorkCapabilityStaysForeground(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	workingDir := t.TempDir()
	jobs := shellprocess.New()
	tool := New(nil, &config.RuntimeConfig{Config: config.Config{WorkingDir: workingDir}})
	tool.AttachProcessManager(jobs, true)
	tool.handler.backgroundAfter = 20 * time.Millisecond
	ctx := tools.WithResourceOwner(t.Context(), tools.NewResourceOwner())
	result, err := tool.handler.RunShell(ctx, RunShellArgs{Cmd: "sleep 0.08; echo fallback"}, tools.NopRuntime{})
	require.NoError(t, err)
	require.Contains(t, result.Output, "fallback")
	require.NotContains(t, result.Output, "Background job started")
	require.Zero(t, jobs.ProcessCount())
}
