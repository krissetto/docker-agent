package embeddedchat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	dagentcfg "github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/embeddedchat/defaults"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	dagentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestNewLoadsAgentAndWelcomeMessage(t *testing.T) {
	t.Parallel()
	cfg := []byte(`agents:
  root:
    description: Test agent
    instruction: Be helpful.
    welcome_message: Hello from embedded chat.
    harness:
      type: claude-code
`)

	s, err := New(t.Context(), Config{
		AgentSource: dagentcfg.NewBytesSource("agent.yaml", cfg),
		LoadOpts:    defaults.Opts(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, "Hello from embedded chat.", s.WelcomeMessage())
	require.NotNil(t, s.SessionRuntime())
	require.NotNil(t, s.Conversation())
}

func TestNewRequiresAgentSource(t *testing.T) {
	t.Parallel()
	s, err := New(t.Context(), Config{})
	require.Nil(t, s)
	require.ErrorIs(t, err, ErrAgentSourceRequired)
}

func TestNewRequiresRegistriesForAgentSource(t *testing.T) {
	t.Parallel()
	s, err := New(t.Context(), Config{AgentSource: dagentcfg.NewBytesSource("agent.yaml", []byte("agents:"))})
	require.Nil(t, s)
	require.ErrorIs(t, err, ErrRegistriesRequired)
}

// newCodeBuiltTeam assembles a minimal team in code, the way embedders that
// avoid the YAML loader (and its full registries) do.
func newCodeBuiltTeam() *team.Team {
	root := agent.New("root", "Be helpful.",
		agent.WithModel(stubProvider{}),
		agent.WithWelcomeMessage("Hello from a code-built team."))
	return team.New(team.WithAgents(root))
}

// stubProvider satisfies the model validation for code-built teams; these
// tests never run a completion.
type stubProvider struct{}

func (stubProvider) ID() modelsdev.ID { return modelsdev.ParseIDOrZero("test/stub-model") }
func (stubProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return nil, errors.New("stub provider cannot complete")
}
func (stubProvider) BaseConfig() base.Config { return base.Config{} }

func TestNewFromCodeBuiltTeam(t *testing.T) {
	t.Parallel()
	s, err := New(t.Context(), Config{Team: newCodeBuiltTeam()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, "Hello from a code-built team.", s.WelcomeMessage())
	require.NotNil(t, s.SessionRuntime())
	require.NotNil(t, s.Conversation())
}

func TestConversationsCarryWorkspaceProvenance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	s, err := New(t.Context(), Config{
		Team:          newCodeBuiltTeam(),
		RuntimeConfig: &dagentcfg.RuntimeConfig{Config: dagentcfg.Config{WorkingDir: root}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, root, s.Conversation().WorkingDir)

	require.NoError(t, s.Restart())
	require.Equal(t, root, s.Conversation().WorkingDir, "restarted conversations must keep the workspace root")
}

func TestSessionOptionsOverrideCapturedWorkingDir(t *testing.T) {
	t.Parallel()
	configured := t.TempDir()
	override := t.TempDir()

	s, err := New(t.Context(), Config{
		Team:           newCodeBuiltTeam(),
		RuntimeConfig:  &dagentcfg.RuntimeConfig{Config: dagentcfg.Config{WorkingDir: configured}},
		SessionOptions: []session.Opt{session.WithWorkingDir(override)},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Equal(t, override, s.Conversation().WorkingDir)
}

func TestInitialSessionResumesConversation(t *testing.T) {
	t.Parallel()
	restored := session.New()
	restored.AddMessage(session.UserMessage("earlier prompt"))

	s, err := New(t.Context(), Config{Team: newCodeBuiltTeam(), InitialSession: restored})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.Same(t, restored, s.Conversation(), "the first conversation must be the restored one")

	require.NoError(t, s.Restart())
	require.NotSame(t, restored, s.Conversation(), "Restart must start a fresh conversation")
}

type fakeRuntime struct {
	dagentruntime.UnsupportedSessionHandle
	events chan dagentruntime.Event

	runCtxs       []context.Context
	resumes       []dagentruntime.ResumeRequest
	elicitations  []tools.ElicitationAction
	closed        bool
	stopWakeCalls int
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{events: make(chan dagentruntime.Event, 8)}
}

func (f *fakeRuntime) CreateSession(context.Context, *session.Session, dagentruntime.SessionBinding) (dagentruntime.SessionHandle, error) {
	return f, nil
}
func (f *fakeRuntime) SessionByID(string) (dagentruntime.SessionHandle, error) { return f, nil }
func (f *fakeRuntime) DeleteSession(context.Context, string) error             { return nil }
func (f *fakeRuntime) ID() string                                              { return "session" }
func (f *fakeRuntime) AgentName() string                                       { return "agent" }
func (f *fakeRuntime) Metadata() dagentruntime.SessionMetadata {
	return dagentruntime.SessionMetadata{}
}

func (f *fakeRuntime) Submit(context.Context, dagentruntime.TurnInput) (dagentruntime.Submission, error) {
	return dagentruntime.Submission{SessionID: "session", TurnID: "request"}, nil
}

func (f *fakeRuntime) Retry(ctx context.Context) (dagentruntime.Submission, error) {
	return f.Submit(ctx, dagentruntime.TurnInput{Retry: true})
}

func (f *fakeRuntime) Send(ctx context.Context, input dagentruntime.TurnInput) (dagentruntime.Submission, error) {
	return f.Submit(ctx, input)
}

func (f *fakeRuntime) Observe(ctx context.Context, _ dagentruntime.ObserveOptions) (dagentruntime.Observation, error) {
	f.runCtxs = append(f.runCtxs, ctx)
	out := make(chan dagentruntime.SessionEvent, 8)
	go func() {
		defer close(out)
		for event := range f.events {
			select {
			case out <- dagentruntime.SessionEvent{SessionID: "session", TurnID: "request", Event: event}:
			case <-ctx.Done():
				return
			}
		}
		out <- dagentruntime.SessionEvent{SessionID: "session", TurnID: "request", Event: &dagentruntime.StreamStoppedEvent{}}
	}()
	return dagentruntime.Observation{Events: out, Cancel: func() {}}, nil
}

func (f *fakeRuntime) Status(context.Context) (dagentruntime.SessionStatus, error) {
	return dagentruntime.SessionStatus{}, nil
}

func (f *fakeRuntime) Respond(_ context.Context, response dagentruntime.InteractionResponse) error {
	if response.Kind == dagentruntime.InteractionElicitation {
		f.elicitations = append(f.elicitations, response.Elicitation.Action)
	} else {
		f.resumes = append(f.resumes, response.Resume)
	}
	return nil
}

func (f *fakeRuntime) Cancel(context.Context, string) (dagentruntime.CancelResult, error) {
	f.stopWakeCalls++
	return dagentruntime.CancelResult{Outcome: dagentruntime.CancelAccepted}, nil
}

func (f *fakeRuntime) Close() error {
	f.closed = true
	return nil
}

type fakeSupervisor struct{ runtime *fakeRuntime }

func (s *fakeSupervisor) Runtime() dagentruntime.SessionRuntime { return s.runtime }
func (s *fakeSupervisor) Shutdown(context.Context) error {
	s.runtime.closed = true
	return nil
}

func newTestSession(rt *fakeRuntime) *Session {
	sess := session.New(session.WithAgentName("agent"))
	return &Session{supervisor: &fakeSupervisor{runtime: rt}, rt: rt, handle: rt, conversation: sess}
}

func TestTranslateRuntimeEvent(t *testing.T) {
	t.Parallel()
	call := tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "tool"}}
	def := tools.Tool{Name: "tool"}

	event, ok := TranslateRuntimeEvent(dagentruntime.AgentChoice("agent", "session", "hello"))
	require.True(t, ok)
	require.Equal(t, "hello", event.Text)

	event, ok = TranslateRuntimeEvent(dagentruntime.ToolCall(call, def, "agent"))
	require.True(t, ok)
	require.Equal(t, call, event.Tool.Call)
	require.Equal(t, def, event.Tool.Def)
	require.False(t, event.Tool.Finished)

	event, ok = TranslateRuntimeEvent(dagentruntime.ToolCallResponse("call-1", def, tools.ResultError("boom"), "boom", "agent"))
	require.True(t, ok)
	require.Equal(t, "call-1", event.Tool.Call.ID)
	require.True(t, event.Tool.Finished)
	require.True(t, event.Tool.IsError)

	_, ok = TranslateRuntimeEvent(dagentruntime.AgentChoice("agent", "session", ""))
	require.False(t, ok)
}

func TestSessionSendCancellationClosesWithoutDone(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)
	ctx, cancel := context.WithCancel(t.Context())

	out, err := s.Send(ctx, "hi")
	require.NoError(t, err)
	cancel()

	assertClosed(t, out)
	close(rt.events)
}

func TestSessionSendStreamsEventsAndDone(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	require.Empty(t, s.conversation.Messages, "session submission owns transcript mutation")

	rt.events <- dagentruntime.AgentChoice("agent", s.conversation.ID, "hello")
	require.Equal(t, "hello", receiveEvent(t, out).Text)

	close(rt.events)
	event := receiveEvent(t, out)
	require.True(t, event.Done)
	assertClosed(t, out)
}

func TestSessionSendSurfacesConfirmationAndConfirmResumesRuntime(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "use tool")
	require.NoError(t, err)

	call := tools.ToolCall{ID: "call-1", Function: tools.FunctionCall{Name: "write_file"}}
	def := tools.Tool{Name: "write_file"}
	rt.events <- dagentruntime.ToolCallConfirmation(call, def, "agent", nil)

	event := receiveEvent(t, out)
	require.NotNil(t, event.Tool)
	require.True(t, event.Tool.NeedsConfirmation)
	require.Equal(t, call, event.Tool.Call)

	require.NoError(t, s.Confirm(t.Context(), dagentruntime.ResumeApproveTool("write_file(*)")))
	require.Len(t, rt.resumes, 1)
	require.Equal(t, dagentruntime.ResumeTypeApproveTool, rt.resumes[0].Type)
	require.Equal(t, "write_file(*)", rt.resumes[0].ToolName)
}

func TestSessionSendHandlesRuntimeErrorWithoutDone(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	rt.events <- dagentruntime.Error("boom")

	event := receiveEvent(t, out)
	require.EqualError(t, event.Err, "boom")

	rt.events <- dagentruntime.AgentChoice("agent", s.conversation.ID, "ignored")
	close(rt.events)
	assertClosed(t, out)
}

func TestSessionSendDeclinesElicitationAndRejectsMaxIterations(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	rt.events <- dagentruntime.ElicitationRequest("authorize", "url", nil, "https://example.com", "id", "", "sess", nil, "agent")
	rt.events <- dagentruntime.MaxIterationsReached(3)
	close(rt.events)

	require.True(t, receiveEvent(t, out).Done)
	require.Equal(t, []tools.ElicitationAction{"decline"}, rt.elicitations)
	require.Len(t, rt.resumes, 1)
	require.Equal(t, dagentruntime.ResumeTypeReject, rt.resumes[0].Type)
}

func TestSessionSendRejectsConcurrentRun(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)

	ctx, cancel := context.WithCancel(t.Context())
	_, err := s.Send(ctx, "first")
	require.NoError(t, err)

	out, err := s.Send(t.Context(), "second")
	require.Nil(t, out)
	require.ErrorIs(t, err, ErrRunActive)

	cancel()
	close(rt.events)
}

func TestSessionRejectsOperationsAfterClose(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)

	require.NoError(t, s.Close())
	out, err := s.Send(t.Context(), "hi")
	require.Nil(t, out)
	require.ErrorIs(t, err, ErrClosed)
	require.ErrorIs(t, s.Restart(), ErrClosed)
	require.ErrorIs(t, s.Confirm(t.Context(), dagentruntime.ResumeApprove()), ErrClosed)

	close(rt.events)
}

func TestSessionCloseCancelsActiveRunAndClosesRuntime(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)

	_, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	require.Len(t, rt.runCtxs, 1)

	require.NoError(t, s.Close())
	require.True(t, rt.closed)
	assert.Zero(t, rt.stopWakeCalls, "runtime Close owns final session teardown")
	require.Eventually(t, func() bool {
		return errors.Is(rt.runCtxs[0].Err(), context.Canceled)
	}, time.Second, time.Millisecond)

	close(rt.events)
}

func TestSessionRestartKeepsRunActiveUntilRuntimeStops(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)

	out, err := s.Send(t.Context(), "first")
	require.NoError(t, err)
	require.NoError(t, s.Restart())

	next, err := s.Send(t.Context(), "second")
	require.Nil(t, next)
	require.ErrorIs(t, err, ErrRunActive)

	close(rt.events)
	assertClosed(t, out)

	next, err = s.Send(t.Context(), "second")
	require.NoError(t, err)
	require.True(t, receiveEvent(t, next).Done)
}

func TestSessionRestartCancelsRunAndReplacesConversation(t *testing.T) {
	t.Parallel()
	rt := newFakeRuntime()
	s := newTestSession(rt)

	_, err := s.Send(t.Context(), "hi")
	require.NoError(t, err)
	oldSession := s.conversation

	require.NoError(t, s.Restart())
	assert.Equal(t, 1, rt.stopWakeCalls)
	require.NotSame(t, oldSession, s.conversation)
	require.Empty(t, s.conversation.Messages)
	require.Eventually(t, func() bool {
		return errors.Is(rt.runCtxs[0].Err(), context.Canceled)
	}, time.Second, time.Millisecond)

	close(rt.events)
}

func receiveEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-ch:
		require.True(t, ok)
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for embedded chat event")
		return Event{}
	}
}

func assertClosed(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case event, ok := <-ch:
		require.False(t, ok, "unexpected event: %#v", event)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for embedded chat stream to close")
	}
}

func (f *fakeRuntime) Release(context.Context) error { return nil }

func (f *fakeRuntime) Attach(ctx context.Context, options dagentruntime.ObserveOptions) (dagentruntime.Observation, error) {
	return f.Observe(ctx, options)
}

func (f *fakeRuntime) UpdateTitle(context.Context, string) error { return nil }

func (f *fakeRuntime) Steer(ctx context.Context, input dagentruntime.TurnInput) (dagentruntime.Submission, error) {
	return f.Send(ctx, input)
}
