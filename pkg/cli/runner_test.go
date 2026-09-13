package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gotest.tools/v3/assert"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/jscommands"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestMain(m *testing.M) {
	// PrepareUserMessage tests exercise ${...} expansion without going
	// through Run, which normally does this registration.
	jscommands.Register()
	os.Exit(m.Run())
}

// mockRuntime is the command source and session registry/session the CLI runner
// drives in tests.
type mockRuntime struct {
	events    []runtime.Event
	commands  types.Commands
	observeFn func(context.Context, runtime.ObserveOptions) (runtime.Observation, error)

	mu                    sync.Mutex
	resumes               []runtime.ResumeRequest
	elicitationDeclines   int
	elicitationLastAction tools.ElicitationAction
	title                 string
	edits                 []runtime.SessionEdit
}

func (m *mockRuntime) AgentCommands(context.Context, string) (types.Commands, error) {
	return m.commands, nil
}

func (m *mockRuntime) AgentTools(context.Context, string) ([]tools.Tool, error) {
	return nil, nil
}

func (m *mockRuntime) CreateSession(context.Context, *session.Session, runtime.SessionBinding) (runtime.SessionHandle, error) {
	return cliSession{rt: m}, nil
}

func (m *mockRuntime) SessionByID(string) (runtime.SessionHandle, error) {
	return cliSession{rt: m}, nil
}
func (m *mockRuntime) DeleteSession(context.Context, string) error { return nil }
func (m *mockRuntime) ID() string                                  { return "session" }
func (m *mockRuntime) AgentName() string                           { return "test" }
func (m *mockRuntime) Metadata() runtime.SessionMetadata           { return runtime.SessionMetadata{} }

func (m *mockRuntime) Submit(context.Context, runtime.TurnInput) (runtime.Submission, error) {
	return runtime.Submission{SessionID: "session", TurnID: "request"}, nil
}

func (m *mockRuntime) Retry(ctx context.Context) (runtime.Submission, error) {
	return m.Submit(ctx, runtime.TurnInput{Retry: true})
}

func (m *mockRuntime) Observe(ctx context.Context, opts runtime.ObserveOptions) (runtime.Observation, error) {
	if m.observeFn != nil {
		return m.observeFn(ctx, opts)
	}
	ch := make(chan runtime.SessionEvent, len(m.events)+1)
	for _, event := range m.events {
		ch <- runtime.SessionEvent{SessionID: "session", TurnID: "request", Event: event}
	}
	ch <- runtime.SessionEvent{SessionID: "session", TurnID: "request", Event: &runtime.StreamStoppedEvent{}}
	close(ch)
	return runtime.Observation{Events: ch, Cancel: func() {}}, nil
}

func (m *mockRuntime) Status(context.Context) (runtime.SessionStatus, error) {
	return runtime.SessionStatus{}, nil
}

func (m *mockRuntime) Respond(_ context.Context, response runtime.InteractionResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if response.Kind == runtime.InteractionElicitation {
		m.elicitationDeclines++
		m.elicitationLastAction = response.Elicitation.Action
	} else {
		m.resumes = append(m.resumes, response.Resume)
	}
	return nil
}

func (m *mockRuntime) Cancel(context.Context, string) (runtime.CancelResult, error) {
	return runtime.CancelResult{Outcome: runtime.CancelAccepted}, nil
}

func (m *mockRuntime) getResumes() []runtime.ResumeRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]runtime.ResumeRequest, len(m.resumes))
	copy(result, m.resumes)
	return result
}

func maxIterEvent(maxIter int) *runtime.MaxIterationsReachedEvent {
	return &runtime.MaxIterationsReachedEvent{
		Type:          "max_iterations_reached",
		MaxIterations: maxIter,
	}
}

func TestMaxIterationsAutoApproveInYoloMode(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{maxIterEvent(60)},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true}

	err := Run(t.Context(), out, cfg, rt, cliSessions{rt}, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), 1)
	assert.Equal(t, resumes[0].Type, runtime.ResumeTypeApprove)
}

func TestMaxIterationsAutoApproveSafetyCap(t *testing.T) {
	t.Parallel()

	// Emit maxAutoExtensions+1 events to trigger the safety cap
	events := make([]runtime.Event, maxAutoExtensions+1)
	for i := range events {
		events[i] = maxIterEvent(60 + i*10)
	}

	rt := &mockRuntime{events: events}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true}

	err := Run(t.Context(), out, cfg, rt, cliSessions{rt}, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), maxAutoExtensions+1)

	// First maxAutoExtensions should be approved
	for i := range maxAutoExtensions {
		assert.Equal(t, resumes[i].Type, runtime.ResumeTypeApprove,
			"extension %d should be approved", i+1)
	}
	// Last one should be rejected (safety cap)
	assert.Equal(t, resumes[maxAutoExtensions].Type, runtime.ResumeTypeReject,
		"extension beyond cap should be rejected")
}

func TestMaxIterationsAutoApproveJSONMode(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{maxIterEvent(60)},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true, OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, cliSessions{rt}, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), 1)
	assert.Equal(t, resumes[0].Type, runtime.ResumeTypeApprove)
}

func TestMaxIterationsRejectInJSONModeWithoutYolo(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{maxIterEvent(60)},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: false, OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, cliSessions{rt}, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), 1)
	assert.Equal(t, resumes[0].Type, runtime.ResumeTypeReject)
}

// JSON mode has no stdin user — a ToolCallConfirmationEvent must be
// rejected even under --yolo (AutoApprove=true), because the event
// only fires when a preempt-yolo hook overrode yolo. Without this,
// eval containers hang on the Resume channel.
func TestToolCallConfirmationRejectedInJSONModeUnderYolo(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{
			&runtime.ToolCallConfirmationEvent{
				Type: "tool_call_confirmation",
				ToolCall: tools.ToolCall{
					ID:       "call-1",
					Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"rm -rf /tmp/x"}`},
				},
			},
		},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true, OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, cliSessions{rt}, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), 1, "one Resume expected")
	assert.Equal(t, resumes[0].Type, runtime.ResumeTypeReject, "JSON+yolo must reject confirmation events")
}

func TestElicitationAutoDeclineInJSONMode(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{
			&runtime.ElicitationRequestEvent{
				Type:    "elicitation_request",
				Message: "Please authorize",
				Meta:    map[string]any{"docker-agent/server_url": "https://example.com"},
			},
		},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, cliSessions{rt}, sess, []string{"hello"})
	assert.NilError(t, err)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, rt.elicitationDeclines, 1)
	assert.Equal(t, rt.elicitationLastAction, tools.ElicitationAction("decline"))
}

func TestMaxIterationsSafetyCapJSONMode(t *testing.T) {
	t.Parallel()

	events := make([]runtime.Event, maxAutoExtensions+1)
	for i := range events {
		events[i] = maxIterEvent(60 + i*10)
	}

	rt := &mockRuntime{events: events}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()
	cfg := Config{AutoApprove: true, OutputJSON: true}

	err := Run(t.Context(), out, cfg, rt, cliSessions{rt}, sess, []string{"hello"})
	assert.NilError(t, err)

	resumes := rt.getResumes()
	assert.Equal(t, len(resumes), maxAutoExtensions+1)

	for i := range maxAutoExtensions {
		assert.Equal(t, resumes[i].Type, runtime.ResumeTypeApprove)
	}
	assert.Equal(t, resumes[maxAutoExtensions].Type, runtime.ResumeTypeReject)
}

// TestPrepareUserMessage_AgentSwitching tests that PrepareUserMessage correctly
// handles agent-switching commands and returns empty messages on switch failures.
func TestPrepareUserMessage_AgentSwitching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		userInput       string
		commandAgent    string
		expectedContent string
		expectedAttach  string
		expectError     bool
	}{
		{
			name:            "agent switch succeeds with trailing args",
			userInput:       "/plan design a login flow",
			commandAgent:    "planner",
			expectedContent: "design a login flow",
			expectedAttach:  "",
			expectError:     true,
		},
		{
			name:            "agent switch succeeds without trailing args",
			userInput:       "/plan",
			commandAgent:    "planner",
			expectedContent: "",
			expectedAttach:  "",
			expectError:     true,
		},
		{
			name:            "agent switch fails - returns error",
			userInput:       "/plan design a login flow",
			commandAgent:    "planner",
			expectedContent: "",
			expectedAttach:  "",
			expectError:     true,
		},
		{
			name:            "non-agent command - no switch",
			userInput:       "/test regular command",
			commandAgent:    "",
			expectedContent: "This is the test instruction regular command",
			expectedAttach:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			commands := make(types.Commands)
			if tt.commandAgent != "" {
				commands["plan"] = types.Command{Description: "Hand off to the planner", Agent: tt.commandAgent}
			} else {
				commands["test"] = types.Command{Instruction: "This is the test instruction"}
			}
			rt := &mockRuntime{commands: commands}

			msg, attachPath, err := PrepareUserMessage(t.Context(), rt, "test", tt.userInput, "")
			if tt.expectError {
				assert.Assert(t, err != nil, "Expected error but got nil")
				return
			}
			assert.NilError(t, err)

			assert.Equal(t, tt.expectedContent, msg.Message.Content, "Message content mismatch")
			assert.Equal(t, tt.expectedAttach, attachPath, "Attachment path mismatch")
		})
	}
}

func TestPrepareUserMessage_EmptyMessageForAgentOnlyCommand(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{commands: types.Commands{
		"plan": {Agent: "planner"},
	}}

	msg, attachPath, err := PrepareUserMessage(t.Context(), rt, "test", "/plan", "")
	assert.ErrorContains(t, err, "immutable session binding")
	assert.Assert(t, msg == nil)
	assert.Equal(t, "", attachPath)
}

// TestPrepareUserMessage_CommandResolution tests that commands are resolved
// correctly before agent switching.
func TestPrepareUserMessage_CommandResolution(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{commands: types.Commands{
		"fix": {Instruction: "Fix the file ${args[0]}"},
	}}

	msg, _, err := PrepareUserMessage(t.Context(), rt, "test", "/fix main.go", "")
	assert.NilError(t, err)

	assert.Equal(t, "Fix the file main.go", msg.Message.Content, "Command should be resolved with args")
}

// swapStdin replaces os.Stdin with a pipe carrying the given content for the
// duration of the test. A pipe is never a terminal, so it also exercises the
// non-TTY stdin paths. Tests using it must not run in parallel: os.Stdin is
// process-global state.
func swapStdin(t *testing.T, content string) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating stdin pipe: %v", err)
	}
	if _, err := w.WriteString(content); err != nil {
		t.Fatalf("writing stdin pipe: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing stdin pipe: %v", err)
	}

	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		_ = r.Close()
	})
}

// A run that would silently do nothing (no message, empty non-TTY stdin, e.g.
// bare `docker agent` in CI) must fail with an actionable error instead of
// exiting 0 with no output (issue #3442).
func TestRunEmptyStdinNonTTYFails(t *testing.T) {
	swapStdin(t, "")

	rt := &mockRuntime{}
	var buf bytes.Buffer
	sess := session.New()

	err := Run(t.Context(), NewPrinter(&buf), Config{}, rt, cliSessions{rt}, sess, nil)
	assert.ErrorContains(t, err, "no message provided and stdin is not a terminal")
	assert.ErrorContains(t, err, "interactive terminal")
}

func TestRunEmptyStdinWithDashFails(t *testing.T) {
	swapStdin(t, "")

	rt := &mockRuntime{}
	var buf bytes.Buffer
	sess := session.New()

	err := Run(t.Context(), NewPrinter(&buf), Config{}, rt, cliSessions{rt}, sess, []string{"-"})
	assert.ErrorContains(t, err, "no message received on stdin")
}

func TestRunPipedStdinStillWorks(t *testing.T) {
	swapStdin(t, "hello agent\n")

	rt := &mockRuntime{
		events: []runtime.Event{&runtime.AgentChoiceEvent{Content: "4"}},
	}
	var buf bytes.Buffer
	sess := session.New()

	err := Run(t.Context(), NewPrinter(&buf), Config{}, rt, cliSessions{rt}, sess, nil)
	assert.NilError(t, err)
	assert.Equal(t, len(sess.GetAllMessages()), 0) // session owns transcript mutation
}

// An ErrorEvent must surface exactly once: returned to the command layer
// (which prints it), never also printed by the runner (issue #3442).
func TestErrorEventReturnedNotPrinted(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{
		events: []runtime.Event{runtime.Error("model failed: HTTP 404")},
	}
	var buf bytes.Buffer
	sess := session.New()

	err := Run(t.Context(), NewPrinter(&buf), Config{}, rt, cliSessions{rt}, sess, []string{"hello"})
	assert.ErrorContains(t, err, "model failed: HTTP 404")

	var runtimeErr RuntimeError
	assert.Equal(t, errors.As(err, &runtimeErr), true)
	assert.Equal(t, strings.Contains(buf.String(), "model failed"), false)
}

// A non-OAuth MCP elicitation must be declined in CLI mode without abandoning the event
// stream: the runtime only makes progress while the consumer drains, so
// returning early would stall the follow-up events (redirect warning,
// assistant response) and lose the turn. The unbuffered stream below makes
// the test fail (bounded, not wedged) if Run stops consuming after the
// decline.
func TestNonOAuthElicitationDeclinedAndStreamDrained(t *testing.T) {
	t.Parallel()

	drained := make(chan struct{})
	rt := &mockRuntime{
		observeFn: func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
			ch := make(chan runtime.SessionEvent) // unbuffered: every send needs a live consumer
			go func() {
				defer close(ch)
				defer close(drained)
				ch <- runtime.SessionEvent{TurnID: "request", Event: &runtime.ElicitationRequestEvent{Type: "elicitation_request", Message: "Choose a deployment region"}}
				ch <- runtime.SessionEvent{TurnID: "request", Event: runtime.Warning("The deployment choice was declined", "test")}
				ch <- runtime.SessionEvent{TurnID: "request", Event: runtime.AgentChoice("test", "sess", "Continuing without deployment.")}
				ch <- runtime.SessionEvent{TurnID: "request", Event: &runtime.StreamStoppedEvent{}}
			}()
			return runtime.Observation{Events: ch, Cancel: func() {}}, nil
		},
	}

	var buf bytes.Buffer
	out := NewPrinter(&buf)
	sess := session.New()

	err := Run(t.Context(), out, Config{}, rt, cliSessions{rt}, sess, []string{"hello"})
	assert.NilError(t, err)

	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the CLI stopped draining the stream after declining the elicitation")
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, rt.elicitationDeclines, 1)
	assert.Equal(t, rt.elicitationLastAction, tools.ElicitationAction("decline"))
	assert.Check(t, strings.Contains(buf.String(), "deployment choice was declined"),
		"the warning must be surfaced: %q", buf.String())
	assert.Check(t, strings.Contains(buf.String(), "Continuing without deployment."),
		"the assistant response must still be printed: %q", buf.String())
}
func (m *mockRuntime) Release(context.Context) error { return nil }

func (m *mockRuntime) UpdateTitle(_ context.Context, title string) error {
	m.title = title
	return nil
}

type cliSessions struct{ rt *mockRuntime }

func (r cliSessions) CreateSession(context.Context, *session.Session, runtime.SessionBinding) (runtime.SessionHandle, error) {
	return cliSession{rt: r.rt}, nil
}

func (r cliSessions) SessionByID(string) (runtime.SessionHandle, error) {
	return cliSession{rt: r.rt}, nil
}
func (r cliSessions) DeleteSession(context.Context, string) error { return nil }

type cliSession struct {
	runtime.UnsupportedSessionHandle

	rt *mockRuntime
}

func (a cliSession) ID() string                        { return a.rt.ID() }
func (a cliSession) AgentName() string                 { return a.rt.AgentName() }
func (a cliSession) Metadata() runtime.SessionMetadata { return a.rt.Metadata() }
func (a cliSession) Submit(c context.Context, i runtime.TurnInput) (runtime.Submission, error) {
	return a.rt.Submit(c, i)
}
func (a cliSession) Retry(c context.Context) (runtime.Submission, error) { return a.rt.Retry(c) }

func (a cliSession) Steer(c context.Context, i runtime.TurnInput) (runtime.Submission, error) {
	return a.rt.Submit(c, i)
}

func (a cliSession) Observe(c context.Context, o runtime.ObserveOptions) (runtime.Observation, error) {
	return a.rt.Observe(c, o)
}

func (a cliSession) Status(c context.Context) (runtime.SessionStatus, error) {
	return a.rt.Status(c)
}

func (a cliSession) Respond(c context.Context, r runtime.InteractionResponse) error {
	return a.rt.Respond(c, r)
}

func (a cliSession) Cancel(c context.Context, id string) (runtime.CancelResult, error) {
	return a.rt.Cancel(c, id)
}
func (a cliSession) Release(c context.Context) error               { return a.rt.Release(c) }
func (a cliSession) UpdateTitle(c context.Context, t string) error { return a.rt.UpdateTitle(c, t) }

func (a cliSession) AwaitTurn(context.Context, string) error { return nil }

func (a cliSession) Edit(_ context.Context, edit runtime.SessionEdit) (*session.Session, error) {
	a.rt.edits = append(a.rt.edits, edit)
	return session.New(), nil
}

func TestRunEditsCanonicalTitleAndAttachmentNotOriginalSession(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "context.txt")
	assert.NilError(t, os.WriteFile(path, []byte("attached context"), 0o600))
	rt := &mockRuntime{}
	original := session.New(session.WithTitle("original"))
	var output bytes.Buffer
	assert.NilError(t, Run(t.Context(), NewPrinter(&output), Config{AttachmentPath: path}, rt, cliSessions{rt}, original, []string{"hello"}))
	assert.Equal(t, rt.title, "Running agent")
	assert.Equal(t, len(rt.edits), 1)
	assert.Equal(t, rt.edits[0].Kind, runtime.SessionEditAttachment)
	assert.Equal(t, rt.edits[0].AttachmentPath, path)
	assert.Equal(t, original.TitleSnapshot(), "original")
	assert.Equal(t, len(original.AttachedFilesSnapshot()), 0)
}
