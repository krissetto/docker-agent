package tools

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Capability identifies an optional runtime facility a tool can probe for
// via [Runtime.Supports] before relying on it.
type Capability string

const (
	// CapabilityOutput indicates [Runtime.EmitOutput] streams incremental
	// output to a live consumer (UI, event feed).
	CapabilityOutput Capability = "output"
	// CapabilityRecall indicates [Runtime.Recall] can inject messages into
	// a running agent loop.
	CapabilityRecall Capability = "recall"
	// CapabilityBackgroundWork indicates the host can retain shell jobs after a tool call returns.
	CapabilityBackgroundWork Capability = "background_work"
)

// ConfirmedRun describes an action a tool has to perform on the user's machine
// outside of its own tool call — today a shell command embedded in the body of
// a skill the tool just loaded. The host presents it as a tool call so the
// user learns about it and can reject it.
type ConfirmedRun struct {
	// ToolName attributes the action to a tool: the host classifies,
	// permission-matches and renders it as a call to that tool, so an
	// embedded shell command uses the shell tool's name.
	ToolName string
	// Args are the tool-call arguments describing the action, e.g.
	// {"cmd": "git status"}.
	Args map[string]any
	// Metadata carries extra key/value context for the approval prompt,
	// such as the skill the action originates from.
	Metadata map[string]string
}

// NestedToolInvoker lets a composite tool execute an inner declaration through
// the host's complete dispatch pipeline, rather than invoking its raw handler.
type NestedToolInvoker interface {
	InvokeTool(ctx context.Context, tool Tool, call ToolCall) (*ToolCallResult, error)
}

// Runtime is the tool-side handle to the hosting agent runtime, passed
// explicitly to every [ToolHandler]. Implementations must remain valid after
// the handler returns so background work can hold the handle; every method
// takes ctx from the caller instead of capturing one.
//
// Hosts without an agent loop pass [NopRuntime].
type Runtime interface {
	// EmitOutput streams incremental output for the current tool call.
	EmitOutput(ctx context.Context, output string)
	// Recall injects a message into the agent loop, waking it if idle.
	// Typically called when background work completes after the tool call
	// that started it has already returned.
	Recall(ctx context.Context, message string) error
	// ConfirmAndRun obtains the user's approval for run and, once granted,
	// calls exec and returns its output. Denied and unsupported actions
	// return [ErrConfirmationDenied] / [ErrConfirmationUnsupported] without
	// calling exec.
	ConfirmAndRun(ctx context.Context, run ConfirmedRun, exec func(context.Context, ConfirmedRun) (string, error)) (string, error)
	// Supports reports whether the host provides the named capability,
	// letting tools fail fast before starting background work.
	Supports(capability Capability) bool
}

// BackgroundProcessState is the authoritative lifecycle state of an adopted shell process.
type BackgroundProcessState string

const (
	BackgroundProcessRunning   BackgroundProcessState = "running"
	BackgroundProcessCompleted BackgroundProcessState = "completed"
	BackgroundProcessStopped   BackgroundProcessState = "stopped"
	BackgroundProcessFailed    BackgroundProcessState = "failed"
)

// BackgroundProcess describes a shell process that the process manager actually adopted.
type BackgroundProcess struct {
	ID         string                 `json:"id"`
	SessionID  string                 `json:"session_id"`
	AgentName  string                 `json:"agent_name"`
	ToolCallID string                 `json:"tool_call_id"`
	Command    string                 `json:"command"`
	Cwd        string                 `json:"cwd"`
	StartedAt  time.Time              `json:"started_at"`
	FinishedAt time.Time              `json:"finished_at,omitzero"`
	State      BackgroundProcessState `json:"state"`
}

func (p BackgroundProcess) MarshalJSON() ([]byte, error) {
	var finishedAt *time.Time
	if !p.FinishedAt.IsZero() {
		finished := p.FinishedAt
		finishedAt = &finished
	}
	return json.Marshal(struct {
		ID         string                 `json:"id"`
		SessionID  string                 `json:"session_id"`
		AgentName  string                 `json:"agent_name"`
		ToolCallID string                 `json:"tool_call_id"`
		Command    string                 `json:"command"`
		Cwd        string                 `json:"cwd"`
		StartedAt  time.Time              `json:"started_at"`
		FinishedAt *time.Time             `json:"finished_at,omitempty"`
		State      BackgroundProcessState `json:"state"`
	}{p.ID, p.SessionID, p.AgentName, p.ToolCallID, p.Command, p.Cwd, p.StartedAt, finishedAt, p.State})
}

// BackgroundWork tracks an adopted process until its completion guidance has
// been admitted and its retained environment can be released. Close must be
// called exactly once after Complete (or when completion is intentionally
// suppressed during session teardown).
type BackgroundWork interface {
	Complete(message string) error
	Cancel()
	Close() error
}

// BackgroundProcessIdentity supplies stable ownership details for a process that the manager adopts.
type BackgroundProcessIdentity interface {
	BackgroundProcessIdentity() (sessionID, agentName, toolCallID string)
}

// BackgroundProcessLifecycle is an optional BackgroundWork extension implemented by hosts that
// publish process state. The process manager invokes Adopt only after retaining a live process,
// and Finish exactly once when that process reaches a terminal state.
type BackgroundProcessLifecycle interface {
	AdoptBackgroundProcess(process BackgroundProcess) error
	FinishBackgroundProcess(state BackgroundProcessState, finishedAt time.Time)
}

// BackgroundWorkRuntime is an optional capability used by tools that hand
// a still-running process to a host-owned background-job manager.
type BackgroundWorkRuntime interface {
	BeginBackgroundWork(ctx context.Context) (BackgroundWork, error)
}

// ErrRecallNotSupported is returned by [Runtime.Recall] implementations that
// cannot steer an agent loop.
var ErrRecallNotSupported = errors.New("recall is not supported by this host")
var ErrBackgroundWorkNotSupported = errors.New("background work is not supported by this host")

// ErrConfirmationDenied is returned by [Runtime.ConfirmAndRun] when the user,
// a permission rule or the session's safety mode refused the action.
var ErrConfirmationDenied = errors.New("action was not approved")

// ErrConfirmationUnsupported is returned by [Runtime.ConfirmAndRun] when the
// host has no way to ask the user. It is a denial too: an unattended host must
// not silently run what it cannot get approved.
var ErrConfirmationUnsupported = errors.New("cannot ask for approval here, so the action was skipped")

// ErrCallTimeout is returned when a tool call exceeds its configured
// call_timeout. Wrapped with the elapsed duration via fmt.Errorf("%w after
// %s", ErrCallTimeout, ...) — use errors.Is to detect it.
var ErrCallTimeout = errors.New("tool call timed out")

// NopRuntime is the [Runtime] for hosts without an agent loop (prompt
// templates, tests). EmitOutput is a no-op, Recall fails with
// [ErrRecallNotSupported], ConfirmAndRun refuses with
// [ErrConfirmationUnsupported], and no capability is supported.
type NopRuntime struct{}

var _ Runtime = NopRuntime{}

func (NopRuntime) EmitOutput(context.Context, string) {}

func (NopRuntime) Recall(context.Context, string) error { return ErrRecallNotSupported }

func (NopRuntime) ConfirmAndRun(context.Context, ConfirmedRun, func(context.Context, ConfirmedRun) (string, error)) (string, error) {
	return "", ErrConfirmationUnsupported
}

func (NopRuntime) Supports(Capability) bool { return false }
