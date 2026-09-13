package chat

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// newTestChatPage creates a session-bound page for presentation-focused tests.
func newTestChatPage(t *testing.T) *chatPage {
	t.Helper()
	sess := session.New()
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.working = true
	return p
}

type immediateCommandMsg struct{ arg string }

type queueTestServices struct {
	skillset *skillstool.ToolSet
}

// queueTestRuntime preserves the shared fixture name while exposing only app.Services.
type queueTestRuntime = queueTestServices

func (s queueTestServices) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}

func (s queueTestServices) CurrentAgentTools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (s queueTestServices) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }

func (s queueTestServices) RestartToolset(context.Context, string) error                         { return nil }
func (s queueTestServices) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {}
func (s queueTestServices) EmitAgentInfo(context.Context, runtime.EventSink)                     {}
func (s queueTestServices) ResetStartupInfo()                                                    {}
func (s queueTestServices) SessionStore() session.Store                                          { return nil }

func (s queueTestServices) PermissionsInfo() *runtime.PermissionsInfo { return nil }

func (s queueTestServices) CurrentAgentSkillsToolset() *skillstool.ToolSet { return s.skillset }

func (s queueTestServices) CurrentMCPPrompts(context.Context) map[string]tools.PromptInfo {
	return nil
}

func (s queueTestServices) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (s queueTestServices) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return nil
}
func (s queueTestServices) OnToolsChanged(func(runtime.Event))    {}
func (s queueTestServices) OnBackgroundEvent(func(runtime.Event)) {}

type sessionTestSession struct {
	runtime.UnsupportedSessionHandle

	mu             sync.Mutex
	id             string
	state          runtime.SessionState
	submits        []runtime.TurnInput
	sends          []runtime.TurnInput
	retries        int
	sendErr        error
	submitErr      error
	observation    runtime.Observation
	compactPrompts []string
	compactErr     error
	steerQueued    bool
	cancelable     map[string]bool
	withdrawErr    error
}

func (s *sessionTestSession) CancelPendingMessage(_ context.Context, turnID string) (bool, error) {
	if s.withdrawErr != nil {
		return false, s.withdrawErr
	}
	if !s.cancelable[turnID] {
		return false, nil
	}
	delete(s.cancelable, turnID)
	return true, nil
}

func (s *sessionTestSession) ID() string      { return s.id }
func (*sessionTestSession) AgentName() string { return "root" }
func (s *sessionTestSession) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: s.id, AgentName: "root"}
}

func (s *sessionTestSession) Compact(_ context.Context, prompt string, sink runtime.EventSink) error {
	if s.compactErr != nil {
		return s.compactErr
	}
	s.compactPrompts = append(s.compactPrompts, prompt)
	sink.Emit(runtime.SessionCompactionCompleted(s.id, runtime.CompactionOutcomeApplied, "root"))
	return nil
}

func (s *sessionTestSession) ResolveSkillCommand(_ context.Context, input string) (string, error) {
	if input == "/services please" {
		return "Use the following skill.\n\nUser's request: please\n\n<skill name=\"services\">\nservice instructions\n</skill>", nil
	}
	return "", nil
}

func (s *sessionTestSession) Submit(_ context.Context, input runtime.TurnInput) (runtime.Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.submitErr != nil {
		return runtime.Submission{}, s.submitErr
	}
	s.submits = append(s.submits, input)
	return runtime.Submission{SessionID: s.id, TurnID: "submit"}, nil
}

func (s *sessionTestSession) Retry(context.Context) (runtime.Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retries++
	return runtime.Submission{SessionID: s.id, TurnID: "retry"}, nil
}

func (s *sessionTestSession) Steer(_ context.Context, input runtime.TurnInput) (runtime.Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return runtime.Submission{}, s.sendErr
	}
	s.sends = append(s.sends, input)
	disposition := runtime.SubmissionDisposition("")
	if s.steerQueued {
		disposition = runtime.SubmissionDispositionQueued
	}
	return runtime.Submission{SessionID: s.id, TurnID: "send", Disposition: disposition}, nil
}

func (s *sessionTestSession) Observe(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
	return s.observation, nil
}

func (s *sessionTestSession) Status(context.Context) (runtime.SessionStatus, error) {
	return runtime.SessionStatus{SessionID: s.id, AgentName: "root", State: s.state, Pending: len(s.submits)}, nil
}

func (*sessionTestSession) Respond(context.Context, runtime.InteractionResponse) error {
	return nil
}

func (s *sessionTestSession) Cancel(context.Context, string) (runtime.CancelResult, error) {
	return runtime.CancelResult{SessionID: s.id, Outcome: runtime.CancelAccepted}, nil
}

func (s *sessionTestSession) submitted() []runtime.TurnInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runtime.TurnInput(nil), s.submits...)
}

func (s *sessionTestSession) sent() []runtime.TurnInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runtime.TurnInput(nil), s.sends...)
}

type sessionTestRuntime struct{ handle *sessionTestSession }

func (r *sessionTestRuntime) CreateSession(_ context.Context, sess *session.Session, _ runtime.SessionBinding) (runtime.SessionHandle, error) {
	if r.handle == nil {
		r.handle = &sessionTestSession{id: sess.ID, state: runtime.SessionStateSettled}
	}
	return r.handle, nil
}

func (r *sessionTestRuntime) SessionByID(id string) (runtime.SessionHandle, error) {
	if r.handle != nil && r.handle.id == id {
		return r.handle, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "lookup"}
}
func (*sessionTestRuntime) DeleteSession(context.Context, string) error { return nil }

func newSessionTestApp(t *testing.T, sess *session.Session, services *queueTestServices, handle *sessionTestSession, opts ...app.Opt) (*app.App, *sessionTestSession) {
	t.Helper()
	if services == nil {
		services = &queueTestServices{}
	}
	if handle == nil {
		handle = &sessionTestSession{id: sess.ID, state: runtime.SessionStateSettled}
	}
	sessions := &sessionTestRuntime{handle: handle}
	opts = append([]app.Opt{app.WithRuntimeServices(services)}, opts...)
	return app.New(t.Context(), sessions, sess, runtime.SessionBinding{}, opts...), handle
}

func TestQueueFlow_BusyAgent_ImmediateSlashCommandBypassesQueue(t *testing.T) {
	t.Parallel()

	p := newTestChatPage(t)
	p.commandParser = commands.NewParser(commands.Category{
		Name: "Test",
		Commands: []commands.Item{
			{
				SlashCommand: "/now",
				Immediate:    true,
				Execute: func(arg string) tea.Cmd {
					return func() tea.Msg { return immediateCommandMsg{arg: arg} }
				},
			},
		},
	})

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "/now please"})

	require.Empty(t, p.messageQueue)
	require.NotNil(t, cmd)
	assert.Equal(t, immediateCommandMsg{arg: "please"}, cmd())
}

func TestQueueFlow_BusyAgent_BangCommandBypassesQueue(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), func() *app.App { a, _ := newSessionTestApp(t, sess, nil, nil); return a }(), service.NewSessionState(sess)).(*chatPage)
	p.working = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p.msgCancel = cancel

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "!"})

	require.Empty(t, p.messageQueue)
	require.NotNil(t, cmd)
	select {
	case <-ctx.Done():
		t.Fatal("bang command should not cancel the running stream")
	default:
	}
}

// TestQueueFlow_SkillCommand_DispatchesOnceWithoutLooping guards against a
// regression (introduced by reordering the checks in handleSendMsg for the
// --session-read-only feature) where fork-mode skill slash commands looped
// forever. Such commands resolve by re-dispatching themselves as
// SendMsg{BypassQueue: true}; if handleSendMsg re-parses that bypass message
// it matches the same command again, re-runs Execute, and never reaches
// processMessage (so the skill never actually runs).
//
// The test reproduces the real two-hop flow: the user-typed "/myskill" is
// parsed into an Execute that emits a BypassQueue SendMsg, which is then fed
// back through handleSendMsg. Execute must run exactly once.
func TestQueueFlow_SkillCommand_DispatchesOnceWithoutLooping(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), func() *app.App { a, _ := newSessionTestApp(t, sess, nil, nil); return a }(), service.NewSessionState(sess)).(*chatPage)

	calls := 0
	p.commandParser = commands.NewParser(commands.Category{
		Name: "Skills",
		Commands: []commands.Item{
			{
				SlashCommand: "/myskill",
				Immediate:    true,
				Execute: func(string) tea.Cmd {
					calls++
					return core.CmdHandler(messages.SendMsg{Content: "/myskill", BypassQueue: true})
				},
			},
		},
	})

	// Hop 1: the user types "/myskill". It is parsed and Execute emits a
	// BypassQueue re-dispatch of the same command.
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "/myskill"})
	require.NotNil(t, cmd)
	require.Equal(t, 1, calls)

	redispatch, ok := cmd().(messages.SendMsg)
	require.True(t, ok, "skill Execute should re-dispatch a SendMsg")
	require.True(t, redispatch.BypassQueue, "re-dispatch must bypass the queue")

	// Hop 2: the BypassQueue message must be processed directly, not re-parsed
	// back into the command (which would invoke Execute again and loop).
	_, cmd = p.handleSendMsg(redispatch)
	require.NotNil(t, cmd)
	assert.Empty(t, p.messageQueue)
	assert.Equal(t, 1, calls, "BypassQueue message must not be re-parsed into the command again")
}

func TestReadOnly_RejectsMessages(t *testing.T) {
	t.Parallel()

	sess := session.New()
	a := func() *app.App { a, _ := newSessionTestApp(t, sess, nil, nil, app.WithReadOnly()); return a }()
	require.True(t, a.IsReadOnly())

	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "hello"})

	assert.Empty(t, p.messageQueue)
	assert.NotNil(t, cmd)
}

func TestReadOnly_AllowsSlashCommands(t *testing.T) {
	t.Parallel()

	sess := session.New()
	a := func() *app.App { a, _ := newSessionTestApp(t, sess, nil, nil, app.WithReadOnly()); return a }()
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.commandParser = commands.NewParser(commands.Category{
		Name: "Test",
		Commands: []commands.Item{
			{
				SlashCommand: "/now",
				Immediate:    true,
				Execute: func(arg string) tea.Cmd {
					return func() tea.Msg { return immediateCommandMsg{arg: arg} }
				},
			},
		},
	})

	// Slash commands should still work in read-only mode
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "/now please"})

	require.NotNil(t, cmd)
	assert.Equal(t, immediateCommandMsg{arg: "please"}, cmd())
}

// skillCommandParser mirrors the real skill palette item built in
// BuildCommandCategories: an Immediate slash command whose Execute re-emits
// the same slash text as a BypassQueue SendMsg.
func skillCommandParser(name string) *commands.Parser {
	return commands.NewParser(commands.Category{
		Name: "Skills",
		Commands: []commands.Item{
			{
				SlashCommand: "/" + name,
				Immediate:    true,
				Execute: func(arg string) tea.Cmd {
					input := "/" + name
					if arg = strings.TrimSpace(arg); arg != "" {
						input += " " + arg
					}
					return core.CmdHandler(messages.SendMsg{Content: input, BypassQueue: true})
				},
			},
		},
	})
}

// dispatchTypedSkill reproduces typing a skill slash command in the editor.
// Hop 1: handleSendMsg parses it and Execute re-emits a BypassQueue SendMsg.
// Hop 2: that message is fed back through handleSendMsg, which must route it
// to processMessage instead of re-parsing it into another SendMsg (the loop).
func dispatchTypedSkill(t *testing.T, p *chatPage, content string) {
	t.Helper()

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: content})
	require.NotNil(t, cmd)

	redispatch, ok := cmd().(messages.SendMsg)
	require.True(t, ok, "typed skill should resolve to a re-dispatched SendMsg")
	require.True(t, redispatch.BypassQueue, "re-dispatch must bypass the queue")

	_, cmd = p.handleSendMsg(redispatch)
	require.NotNil(t, cmd)
	if _, loops := cmd().(messages.SendMsg); loops {
		t.Fatal("skill SendMsg was re-emitted: dispatch is looping")
	}
}

// TestHandleSendMsg_SessionNativeSubmit verifies an inline skill is resolved before
// it is submitted to the session. Fork-mode skills are intentionally
// session-native unsupported and must not silently use classic fork execution.
func TestHandleSendMsg_SessionNativeSubmitAndUnsupportedFork(t *testing.T) {
	t.Parallel()

	inline := skillstool.New([]skills.Skill{{Name: "services", Description: "List services", InlineContent: "# Services\nList repository services."}}, t.TempDir())
	sess := session.New()
	a, handle := newSessionTestApp(t, sess, &queueTestServices{skillset: inline}, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.commandParser = skillCommandParser("services")
	dispatchTypedSkill(t, p, "/services please")
	require.Eventually(t, func() bool { return len(handle.submitted()) == 1 }, time.Second, 10*time.Millisecond)
	require.Len(t, handle.submitted(), 1)
	assert.Contains(t, handle.submitted()[0].Content, `<skill name="services">`)
	assert.Contains(t, handle.submitted()[0].Content, "User's request: please")

	fork := skillstool.New([]skills.Skill{{Name: "worker", Description: "Fork", Context: "fork", InlineContent: "work"}}, t.TempDir())
	forkApp, forkHandle := newSessionTestApp(t, session.New(), &queueTestServices{skillset: fork}, nil)
	forkPage := New(animation.NewRuntime(), t.Context(), forkApp, service.NewSessionState(forkApp.Session())).(*chatPage)
	forkPage.commandParser = skillCommandParser("worker")
	dispatchTypedSkill(t, forkPage, "/worker task")
	assert.Empty(t, forkHandle.submitted(), "unsupported fork must not fall back to parent Submit")
}

// TestReadOnly_RejectsBypassQueueCommands ensures resolved skill/agent
// commands (re-dispatched as SendMsg{BypassQueue: true}) are still blocked in
// read-only mode. BypassQueue only skips re-parsing to avoid the command loop;
// it must not let model-bound work slip past the read-only guard.
func TestReadOnly_RejectsBypassQueueCommands(t *testing.T) {
	t.Parallel()

	sess := session.New()
	a := func() *app.App { a, _ := newSessionTestApp(t, sess, nil, nil, app.WithReadOnly()); return a }()
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)

	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "/myskill", BypassQueue: true})

	require.NotNil(t, cmd)
	assert.Empty(t, p.messageQueue)
	assert.False(t, p.working, "read-only must not start processing a BypassQueue message")
	assert.Nil(t, p.msgCancel, "read-only must not start a stream for a BypassQueue message")
}

func TestFollowUpFlow_BusyAgent_UsesSessionSubmit(t *testing.T) {
	t.Parallel()
	sess := session.New()
	a, handle := newSessionTestApp(t, sess, nil, &sessionTestSession{id: sess.ID, state: runtime.SessionStateRunning})
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.working = true
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "do this next", FollowUp: true})
	require.NotNil(t, cmd)
	assert.IsType(t, followUpSentMsg{}, cmd())
	require.Len(t, handle.submitted(), 1)
	assert.Equal(t, "do this next", handle.submitted()[0].Content)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, runtime.SessionStateRunning, status.State)
	assert.Equal(t, 1, status.Pending)
}

func TestSteerFlow_BusyAgent_UsesSessionSend(t *testing.T) {
	t.Parallel()
	sess := session.New()
	a, handle := newSessionTestApp(t, sess, nil, &sessionTestSession{id: sess.ID, state: runtime.SessionStateRunning})
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.working = true
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "extra context"})
	require.NotNil(t, cmd)
	assert.IsType(t, steerSentMsg{}, cmd())
	require.Len(t, handle.sent(), 1)
	assert.Equal(t, "extra context", handle.sent()[0].Content)
}

func TestSteerFlow_QueuedAdmissionUsesFollowUpToastPath(t *testing.T) {
	t.Parallel()
	sess := session.New()
	a, handle := newSessionTestApp(t, sess, nil, &sessionTestSession{id: sess.ID, state: runtime.SessionStateRunning, steerQueued: true})
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.working = true
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "after compact"})
	require.NotNil(t, cmd)
	assert.IsType(t, followUpSentMsg{}, cmd())
	require.Len(t, handle.sent(), 1)
	assert.Empty(t, handle.submitted(), "queued steer remains a single driver admission")
}

func TestSteerFlow_ExplicitAndConfiguredQueueSkipSessionSend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		mode     messages.SendMode
		explicit bool
	}{
		{name: "explicit", explicit: true}, {name: "configured", mode: messages.SendModeQueue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := session.New()
			a, handle := newSessionTestApp(t, sess, nil, &sessionTestSession{id: sess.ID, state: runtime.SessionStateRunning})
			p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess), WithSendMode(tc.mode)).(*chatPage)
			p.working = true
			_, cmd := p.handleSendMsg(messages.SendMsg{Content: "for later", Queue: tc.explicit})
			require.NotNil(t, cmd)
			assert.IsType(t, followUpSentMsg{}, cmd())
			require.Len(t, handle.submitted(), 1)
			assert.Equal(t, "for later", handle.submitted()[0].Content)
			assert.Empty(t, p.messageQueue, "session-backed queue is projected only after admission")
			assert.Empty(t, handle.sent())
		})
	}
}

func TestSteerFlow_SessionSendRejectedDoesNotSubmitOrQueueLocally(t *testing.T) {
	t.Parallel()
	sess := session.New()
	rejected := &runtime.SessionError{Kind: runtime.SessionErrorCapacity, SessionID: sess.ID, Operation: "send", Limit: 1}
	a, handle := newSessionTestApp(t, sess, nil, &sessionTestSession{id: sess.ID, state: runtime.SessionStateRunning, sendErr: rejected})
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.working = true
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "extra context"})
	failed, ok := cmd().(steerFailedMsg)
	require.True(t, ok)
	require.ErrorIs(t, failed.err, rejected)
	_, cmd = p.Update(failed)
	require.NotNil(t, cmd)
	assert.Empty(t, handle.submitted(), "rejected steer must not become a different session command")
	assert.Empty(t, p.messageQueue, "rejected steer must not create local queue truth")
}

func TestFollowUpFlow_SessionSubmitRejectedDoesNotQueueLocally(t *testing.T) {
	t.Parallel()
	sess := session.New()
	rejected := &runtime.SessionError{Kind: runtime.SessionErrorCapacity, SessionID: sess.ID, Operation: "submit", Limit: 1}
	a, handle := newSessionTestApp(t, sess, nil, &sessionTestSession{id: sess.ID, state: runtime.SessionStateRunning, submitErr: rejected})
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.working = true
	_, cmd := p.handleSendMsg(messages.SendMsg{Content: "later", FollowUp: true})
	failed, ok := cmd().(followUpFailedMsg)
	require.True(t, ok)
	require.ErrorIs(t, failed.err, rejected)
	_, cmd = p.Update(failed)
	require.NotNil(t, cmd)
	assert.Empty(t, handle.submitted())
	assert.Empty(t, p.messageQueue, "rejected submit must not create local queue truth")
}

func TestSessionFixtureObserveRetryAndTypedUnsupported(t *testing.T) {
	t.Parallel()
	sess := session.New()
	events := make(chan runtime.SessionEvent)
	close(events)
	handle := &sessionTestSession{id: sess.ID, state: runtime.SessionStateSettled, observation: runtime.Observation{
		Initial: []runtime.SessionSnapshot{{Session: sess.Clone(), Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled}}},
		Events:  events, Cancel: func() {},
	}}
	_, handle = newSessionTestApp(t, sess, nil, handle)
	obs, err := handle.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	assert.Equal(t, sess.ID, obs.Primary().Status.SessionID)
	_, err = handle.Retry(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, handle.retries)
	var sessionErr *runtime.SessionError
	unsupportedFork := &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: sess.ID, Operation: "fork"}
	require.ErrorAs(t, unsupportedFork, &sessionErr)
	assert.Equal(t, runtime.SessionErrorUnsupported, sessionErr.Kind)
}
func (s *sessionTestSession) Release(context.Context) error { return nil }

func (*sessionTestSession) UpdateTitle(context.Context, string) error { return nil }

func TestOptionUpRecallsOnlyConfirmedWithdrawals(t *testing.T) {
	t.Parallel()
	sess := session.New()
	a, handle := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.messageQueue = []queuedMessage{{turnID: "consumed", content: "already sent"}, {turnID: "steer", content: "first"}, {turnID: "follow", content: "second"}}
	handle.cancelable = map[string]bool{"steer": true, "follow": true}
	msgs := runTimerCmd(t, p.restorePendingMessages())
	assert.Contains(t, msgs, messages.RestorePendingMessagesMsg{Content: "first\nsecond"})
	assert.Len(t, p.messageQueue, 3, "only canonical canceled events remove the projection")
	_, _ = p.handleRuntimeEvent(runtime.PendingUserMessageCanceled(sess.ID, "steer", -1))
	_, _ = p.handleRuntimeEvent(runtime.PendingUserMessageCanceled(sess.ID, "follow", -1))
	require.Len(t, p.messageQueue, 1)
	assert.Equal(t, "consumed", p.messageQueue[0].turnID)
}

func TestRecallErrorKeepsCanonicalPendingProjection(t *testing.T) {
	t.Parallel()
	sess := session.New()
	a, handle := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	p.messageQueue = []queuedMessage{{turnID: "pending", content: "keep me"}}
	handle.withdrawErr = runtime.ErrUnsupported
	for _, msg := range runTimerCmd(t, p.restorePendingMessages()) {
		_, restored := msg.(messages.RestorePendingMessagesMsg)
		assert.False(t, restored)
	}
	assert.Len(t, p.messageQueue, 1)
}
