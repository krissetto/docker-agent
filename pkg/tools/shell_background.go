package tools

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// ShellBackgroundHandoffPrefix starts the shell tool result returned when a
// command is adopted as a background process.
const ShellBackgroundHandoffPrefix = "Command continues in the background as process ID "

const (
	shellCompletionPrefix       = "Shell command "
	shellCompletionOutputMarker = "--- Untrusted command output ---\n"
	shellCompletionNoOutput     = "<no output>\n"
	shellCompletionTimeoutLine  = "Error: command timed out after "
)

var (
	shellProcessIDPattern     = regexp.MustCompile(`^job_[0-9]+_[0-9]+$`)
	shellCompletionHeadlineRE = regexp.MustCompile(`^Shell command (job_[0-9]+_[0-9]+) with status (completed|failed|stopped|running|unknown) \(exit code (-?[0-9]+)\)\.$`)
)

// BackgroundProcessOutput is a bounded tail snapshot of an adopted process's output.
type BackgroundProcessOutput struct {
	SessionID  string                 `json:"session_id"`
	ProcessID  string                 `json:"process_id"`
	ToolCallID string                 `json:"tool_call_id"`
	State      BackgroundProcessState `json:"state"`
	// Offset is the byte offset of Output within the process's full output stream.
	Offset int64  `json:"offset"`
	Output string `json:"output"`
	// Total is the number of output bytes retained so far (Offset+len(Output)).
	Total int64 `json:"total"`
	// Truncated reports that the process hit its retained-output limit.
	Truncated bool `json:"truncated,omitempty"`
}

// IsShellProcessID reports whether id has the shell process manager's ID shape.
func IsShellProcessID(id string) bool { return shellProcessIDPattern.MatchString(id) }

// ParseShellBackgroundHandoff extracts the process ID from a shell tool result
// announcing background adoption.
func ParseShellBackgroundHandoff(text string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), ShellBackgroundHandoffPrefix)
	if !ok {
		return "", false
	}
	id, _, ok := strings.Cut(rest, ".")
	if !ok || !IsShellProcessID(id) {
		return "", false
	}
	return id, true
}

// ShellCompletion is the parsed header of a background shell completion notice.
type ShellCompletion struct {
	ProcessID string
	Status    BackgroundProcessState
	ExitCode  int
	TimedOut  bool
	// Output is the (possibly bounded) untrusted command output; "" when none.
	Output string
}

// ParseShellCompletion parses a background shell completion notice. Only the
// trusted header ahead of the untrusted-output marker is inspected, so command
// output cannot spoof another process's completion.
func ParseShellCompletion(text string) (ShellCompletion, bool) {
	header, output, hasOutput := strings.Cut(text, shellCompletionOutputMarker)
	var c ShellCompletion
	found := false
	for line := range strings.SplitSeq(header, "\n") {
		line = strings.TrimSpace(line)
		if !found {
			if !strings.HasPrefix(line, shellCompletionPrefix) {
				continue
			}
			m := shellCompletionHeadlineRE.FindStringSubmatch(line)
			if m == nil {
				return ShellCompletion{}, false
			}
			code, err := strconv.Atoi(m[3])
			if err != nil {
				return ShellCompletion{}, false
			}
			c.ProcessID, c.Status, c.ExitCode, found = m[1], BackgroundProcessState(m[2]), code, true
			continue
		}
		if strings.HasPrefix(line, shellCompletionTimeoutLine) {
			c.TimedOut = true
		}
	}
	if !found {
		return ShellCompletion{}, false
	}
	if hasOutput && output != shellCompletionNoOutput {
		c.Output = output
	}
	return c, true
}

// ErrBackgroundProcessOutputUnsupported reports a host (e.g. a legacy server)
// that cannot serve background process output snapshots.
var ErrBackgroundProcessOutputUnsupported = errors.New("background process output is not supported by this host")

// ErrBackgroundProcessNotFound reports a process that is unknown to, or no
// longer retained by, the requesting session.
var ErrBackgroundProcessNotFound = errors.New("background process not found")

// BackgroundProcessOutputSource is implemented by session handles that can
// serve bounded output tails of their own session's background processes.
type BackgroundProcessOutputSource interface {
	BackgroundProcessOutput(ctx context.Context, processID string, maxBytes int) (BackgroundProcessOutput, error)
}
