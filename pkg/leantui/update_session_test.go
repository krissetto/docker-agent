package leantui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type leanSession struct {
	runtime.UnsupportedSessionHandle

	id             string
	submitted      []runtime.TurnInput
	sent           []runtime.TurnInput
	responses      []runtime.InteractionResponse
	lookups        []string
	stops          int
	level          effort.Level
	modelRef       string
	models         []runtime.ModelChoice
	infoAgent      atomic.Value
	snapshot       *session.Session
	compactPrompts []string
	compactErr     error
	steerQueued    bool
	cancelable     map[string]bool
	withdrawErr    error
	thinkingLevels bool
	sessionEditing bool
	starCalls      int
}

func (a *leanSession) Compact(_ context.Context, prompt string, sink runtime.EventSink) error {
	if a.compactErr != nil {
		return a.compactErr
	}
	a.compactPrompts = append(a.compactPrompts, prompt)
	sink.Emit(runtime.SessionCompactionCompleted(a.id, runtime.CompactionOutcomeApplied, "agent"))
	return nil
}

func (a *leanSession) SupportsModelSwitching() bool                          { return a.models != nil }
func (a *leanSession) AvailableModels(context.Context) []runtime.ModelChoice { return a.models }
func (a *leanSession) SetModel(_ context.Context, modelRef string) error {
	a.modelRef = modelRef
	return nil
}
func (a *leanSession) RefreshModelsCatalog(context.Context) error { return runtime.ErrUnsupported }

func (a *leanSession) SetStarred(_ context.Context, starred bool) error {
	a.starCalls++
	if a.snapshot != nil {
		a.snapshot.Starred = starred
	}
	return nil
}

func (a *leanSession) CurrentThinkingLevel(context.Context) effort.Level { return a.level }

func (a *leanSession) ThinkingLevels(context.Context) []effort.Level {
	return []effort.Level{effort.High}
}

func (a *leanSession) CycleThinkingLevel(context.Context) (effort.Level, error) {
	a.level = effort.High
	return a.level, nil
}

func (a *leanSession) SetThinkingLevel(_ context.Context, level effort.Level) (effort.Level, error) {
	a.level = level
	return level, nil
}

func (a *leanSession) EmitPinnedAgentInfo(_ context.Context, sink runtime.EventSink) {
	a.infoAgent.Store("worker")
	sink.Emit(runtime.AgentInfo("worker", "model", "", "", 0))
}

func (a *leanSession) ID() string        { return a.id }
func (a *leanSession) AgentName() string { return "agent" }
func (a *leanSession) Metadata() runtime.SessionMetadata {
	return runtime.SessionMetadata{SessionID: a.id, ThinkingLevel: a.level, Capabilities: runtime.SessionCapabilities{ModelSwitching: a.models != nil, ThinkingLevels: a.thinkingLevels, SessionEditing: a.sessionEditing, ForkSkills: true}}
}

func (a *leanSession) Retry(context.Context) (runtime.Submission, error) {
	return runtime.Submission{SessionID: a.id, TurnID: "retry"}, nil
}

func (a *leanSession) Submit(_ context.Context, input runtime.TurnInput) (runtime.Submission, error) {
	a.submitted = append(a.submitted, input)
	return runtime.Submission{SessionID: a.id, TurnID: "submit"}, nil
}

func (a *leanSession) Steer(_ context.Context, input runtime.TurnInput) (runtime.Submission, error) {
	a.sent = append(a.sent, input)
	disposition := runtime.SubmissionDisposition("")
	if a.steerQueued {
		disposition = runtime.SubmissionDispositionQueued
	}
	return runtime.Submission{SessionID: a.id, TurnID: "send", Disposition: disposition}, nil
}

func (a *leanSession) Observe(_ context.Context, options runtime.ObserveOptions) (runtime.Observation, error) {
	if options.Tree {
		return runtime.Observation{}, runtime.ErrUnsupported
	}
	return runtime.Observation{Initial: []runtime.SessionSnapshot{{Session: a.snapshot}}, Cancel: func() {}}, nil
}

func (a *leanSession) Status(context.Context) (runtime.SessionStatus, error) {
	return runtime.SessionStatus{SessionID: a.id}, nil
}

func (a *leanSession) Respond(_ context.Context, response runtime.InteractionResponse) error {
	a.responses = append(a.responses, response)
	return nil
}

func (a *leanSession) Cancel(context.Context, string) (runtime.CancelResult, error) {
	a.stops++
	return runtime.CancelResult{SessionID: a.id, Outcome: runtime.CancelAccepted}, nil
}

func (a *leanSession) CancelPendingMessage(_ context.Context, turnID string) (bool, error) {
	if a.withdrawErr != nil {
		return false, a.withdrawErr
	}
	if !a.cancelable[turnID] {
		return false, nil
	}
	delete(a.cancelable, turnID)
	return true, nil
}

type leanSessions struct {
	handle    runtime.SessionHandle
	sessions  []runtime.SessionCatalogEntry
	loadCalls int
}

func (r *leanSessions) CreateSession(context.Context, *session.Session, runtime.SessionBinding) (runtime.SessionHandle, error) {
	return r.handle, nil
}

func (r *leanSessions) SessionByID(sessionID string) (runtime.SessionHandle, error) {
	handle := r.handle.(*leanSession)
	handle.lookups = append(handle.lookups, sessionID)
	return r.handle, nil
}
func (r *leanSessions) DeleteSession(context.Context, string) error { return nil }
func (r *leanSessions) LoadSession(context.Context, string) (runtime.SessionHandle, *session.Session, error) {
	r.loadCalls++
	return r.handle, r.handle.(*leanSession).snapshot, nil
}

func (r *leanSessions) ListSessions(context.Context) ([]runtime.SessionCatalogEntry, error) {
	return r.sessions, nil
}

func sessionModel(t *testing.T) (*model, *leanSession) {
	t.Helper()
	sess := session.New(session.WithAgentName("agent"))
	handle := &leanSession{id: sess.ID, snapshot: sess, models: []runtime.ModelChoice{{Name: "other", Ref: "other/model"}}, thinkingLevels: true, sessionEditing: true}
	m := bareModel(24)
	m.app = app.New(t.Context(), &leanSessions{handle: handle}, sess, runtime.SessionBinding{AgentName: "agent"})
	m.app.Run(t.Context(), func() {}, "active", nil)
	require.Eventually(t, func() bool { return len(handle.submitted) > 0 }, time.Second, time.Millisecond)
	return m, handle
}

func TestFirstMessageBangCommandRunsLocally(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "bang-output")
	m, handle := sessionModel(t)
	handle.submitted = nil

	m.sendFirstMessage(t.Context(), `!printf bang > "`+outputPath+`"`, "")

	output, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	assert.Equal(t, "bang", string(output))
	assert.Empty(t, handle.submitted)
}

func TestSubmitBangCommandRunsImmediatelyWhileBusy(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "bang-output")
	m, handle := sessionModel(t)
	handle.submitted = nil
	m.lifecycle.Status = runtime.SessionStateRunning

	m.submitEditor(t.Context(), `!printf bang > "`+outputPath+`"`)

	output, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	assert.Equal(t, "bang", string(output))
	assert.Empty(t, handle.submitted)
	assert.Empty(t, handle.sent)
	assert.Empty(t, m.queue)
	assert.True(t, m.busy())
}

func TestSubmitBangCommandHonorsReadOnlySession(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "bang-output")
	sess := session.New(session.WithAgentName("agent"))
	handle := &leanSession{id: sess.ID}
	m := bareModel(80)
	m.app = app.New(t.Context(), &leanSessions{handle: handle}, sess, runtime.SessionBinding{AgentName: "agent"}, app.WithReadOnly())

	m.submitEditor(t.Context(), `!printf bang > "`+outputPath+`"`)

	_, err := os.Stat(outputPath)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Empty(t, handle.submitted)
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "This session is read-only.")
}

func TestShiftEnterInsertsNewline(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.screen.Editor.SetText("first line")

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyShiftEnter})

	assert.Equal(t, "first line\n", m.screen.Editor.Text())
}

func TestEscapeWhileIdleDismissesAutocomplete(t *testing.T) {
	t.Parallel()
	m := bareModel(24)
	m.screen.Autocomplete.SetCommands([]ui.Command{{Name: "help"}})
	require.True(t, m.screen.Autocomplete.Sync("/h"))

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})

	assert.False(t, m.screen.Autocomplete.Active)
	assert.False(t, m.quitting)
}

func TestSessionsCommandReportsNoSessionsInCurrentDirectory(t *testing.T) {
	t.Parallel()
	currentDir := t.TempDir()
	sess := session.New(session.WithWorkingDir(currentDir), session.WithAgentName("agent"))
	handle := &leanSession{id: sess.ID}
	sessions := &leanSessions{handle: handle, sessions: []runtime.SessionCatalogEntry{{SessionID: "other", WorkingDir: t.TempDir(), Loadable: true}}}
	m := bareModel(80)
	m.app = app.New(t.Context(), sessions, sess, runtime.SessionBinding{AgentName: "agent"})

	assert.True(t, m.handleSlash(t.Context(), "/sessions", busySubmitSteer))
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "No previous sessions found in this directory")
	assert.False(t, m.screen.Autocomplete.Active)
}

func TestSessionsCommandDoesNotSwitchWhileBusy(t *testing.T) {
	t.Parallel()
	sess := session.New(session.WithWorkingDir(t.TempDir()), session.WithAgentName("agent"))
	handle := &leanSession{id: sess.ID}
	sessions := &leanSessions{handle: handle, sessions: []runtime.SessionCatalogEntry{{SessionID: "other", WorkingDir: sess.WorkingDir, Loadable: true}}}
	m := bareModel(80)
	m.app = app.New(t.Context(), sessions, sess, runtime.SessionBinding{AgentName: "agent"})
	m.lifecycle.Status = runtime.SessionStateRunning

	assert.True(t, m.handleSlash(t.Context(), "/sessions", busySubmitSteer))
	assert.Equal(t, sess.ID, m.app.Session().ID)
	assert.Zero(t, sessions.loadCalls)
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, true, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "Wait for the current response to finish")
}

func TestLeanSessionsFiltersDirectoryAndUnboundRows(t *testing.T) {
	m, handle := sessionModel(t)
	workingDir := t.TempDir()
	m.app.Session().WorkingDir = workingDir
	otherDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "equivalent")
	if err := os.Symlink(workingDir, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	sessions := m.app.SessionRuntime().(*leanSessions)
	sessions.sessions = []runtime.SessionCatalogEntry{
		{SessionID: "same", Title: "Same directory", WorkingDir: workingDir, Loadable: true},
		{SessionID: "linked", Title: "Symlink equivalent", WorkingDir: linkDir, Loadable: true},
		{SessionID: "other", Title: "Other directory", WorkingDir: otherDir, Loadable: true},
		{SessionID: "legacy", Title: "Legacy unbound", WorkingDir: workingDir, Loadable: false},
	}
	handle.snapshot = session.New(session.WithID("same"), session.WithWorkingDir(workingDir), session.WithAgentName("agent"))

	m.handleSessionsCommand(t.Context(), "")

	assert.Equal(t, "/sessions ", m.screen.Editor.Text())
	var values []string
	for range 2 {
		cmd, ok := m.screen.Autocomplete.Current()
		require.True(t, ok)
		values = append(values, cmd.Value)
		m.screen.Autocomplete.MoveDown()
	}
	assert.ElementsMatch(t, []string{"same", "linked"}, values)
}

func TestLeanDirectLoadRejectsCrossDirectoryBeforeHydration(t *testing.T) {
	m, handle := sessionModel(t)
	currentDir := t.TempDir()
	m.app.Session().WorkingDir = currentDir
	sessions := m.app.SessionRuntime().(*leanSessions)
	sessions.sessions = []runtime.SessionCatalogEntry{{SessionID: "cross", WorkingDir: t.TempDir(), Loadable: true}}
	handle.snapshot = session.New(session.WithID("cross"), session.WithWorkingDir(currentDir))
	before := m.screen.Transcript.BlockCount()

	m.handleLoadSession(t.Context(), "cross")

	assert.Equal(t, 0, sessions.loadCalls)
	assert.Equal(t, before+1, m.screen.Transcript.BlockCount())
	assert.NotEqual(t, "cross", m.app.Session().ID)
}

func TestLeanLoadClearsAndHydratesTranscript(t *testing.T) {
	m, handle := sessionModel(t)
	m.screen.Transcript.AddUser("old session")
	loaded := session.New(session.WithID("loaded"), session.WithAgentName("agent"), session.WithUserMessage("loaded user"))
	loaded.AddMessage(session.NewAgentMessage("agent", &chat.Message{Role: chat.MessageRoleAssistant, Content: "loaded answer"}))
	handle.snapshot = loaded
	sessions := m.app.SessionRuntime().(*leanSessions)
	sessions.sessions = []runtime.SessionCatalogEntry{{SessionID: loaded.ID, WorkingDir: loaded.WorkingDir, Loadable: true}}

	m.handleLoadSession(t.Context(), loaded.ID)

	assert.Equal(t, loaded.ID, m.app.Session().ID)
	assert.NotContains(t, m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "old session")
	lines := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, lines, "loaded user")
	assert.Contains(t, lines, "loaded answer")
}

func TestLeanModelCommandUsesActorCapability(t *testing.T) {
	m, handle := sessionModel(t)

	m.handleModelCommand(t.Context(), "other/model")

	assert.Equal(t, "other/model", handle.modelRef)
	assert.Equal(t, 1, m.screen.Transcript.BlockCount())
}

func TestLeanThinkingRefreshUsesPinnedWorker(t *testing.T) {
	m, handle := sessionModel(t)
	handle.infoAgent.Store("")
	_, err := m.app.SetAgentThinkingLevel(t.Context(), effort.High)
	require.NoError(t, err)
	require.Eventually(t, func() bool { v, _ := handle.infoAgent.Load().(string); return v == "worker" }, time.Second, time.Millisecond)
}

func TestBusySubmissionsUseActorSendAndSubmit(t *testing.T) {
	t.Parallel()
	m, handle := sessionModel(t)
	m.lifecycle.Status = runtime.SessionStateRunning

	m.dispatchUserMessage(t.Context(), "steer", "steer", busySubmitSteer)
	m.dispatchUserMessage(t.Context(), "next", "next", busySubmitFollowUp)

	require.Len(t, handle.sent, 1)
	assert.Equal(t, "steer", handle.sent[0].Content)
	assert.Equal(t, "next", handle.submitted[len(handle.submitted)-1].Content)
	assert.Len(t, m.pendingUsers, 2)
}

func TestBusyQueuedSteerIsClassifiedAsFollowUp(t *testing.T) {
	t.Parallel()
	m, handle := sessionModel(t)
	m.lifecycle.Status = runtime.SessionStateRunning
	handle.steerQueued = true
	beforeSubmits := len(handle.submitted)

	m.dispatchUserMessage(t.Context(), "steer", "steer", busySubmitSteer)

	require.Len(t, m.pendingUsers, 1)
	assert.Equal(t, ui.PendingUserFollowUp, m.pendingUsers[0].Kind)
	assert.Len(t, handle.sent, 1)
	assert.Len(t, handle.submitted, beforeSubmits, "queued steer is not retried as submit")
}

func TestInterruptStopsSessionHandle(t *testing.T) {
	t.Parallel()
	m, handle := cancellationModel(t)
	m.lifecycle.Status = runtime.SessionStateRunning

	m.handleInterrupt()

	assert.Equal(t, 1, handle.stops)
	assert.Equal(t, handle.turnID, handle.cancelledTurn)
	assert.True(t, m.cancelMarkerPending, "marker waits for buffered response events")
	assert.Equal(t, 0, m.screen.Transcript.BlockCount())
}

func TestConfirmationRespondsWithSessionAndRequestCorrelation(t *testing.T) {
	t.Parallel()
	m, handle := sessionModel(t)
	m.screen.Confirm = &ui.ConfirmModel{Tool: "shell", SessionID: handle.id, RequestID: "request-1"}

	m.handleConfirmKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'y'}})

	require.Len(t, handle.responses, 1)
	assert.Equal(t, []string{handle.id, handle.id}, handle.lookups)
	assert.Equal(t, "request-1", handle.responses[0].InteractionID)
	assert.Equal(t, runtime.InteractionConfirmation, handle.responses[0].Kind)
	assert.Equal(t, runtime.ResumeTypeApprove, handle.responses[0].Resume.Type)
	assert.Nil(t, m.screen.Confirm)
}

func TestConfirmationEventPreservesActorCorrelation(t *testing.T) {
	t.Parallel()
	m, handle := sessionModel(t)

	m.handleEvent(t.Context(), &runtime.ToolCallConfirmationEvent{
		SessionID: handle.id,
		RequestID: "request-1",
	})

	require.NotNil(t, m.screen.Confirm)
	assert.Equal(t, handle.id, m.screen.Confirm.SessionID)
	assert.Equal(t, "request-1", m.screen.Confirm.RequestID)
}

func TestConfirmationWithoutCorrelationIsVisiblyRejected(t *testing.T) {
	t.Parallel()
	m, handle := sessionModel(t)
	m.screen.Confirm = &ui.ConfirmModel{Tool: "shell"}

	m.handleConfirmKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'n'}})

	assert.Empty(t, handle.responses)
	assert.Equal(t, 1, m.screen.Transcript.BlockCount())
	assert.Nil(t, m.screen.Confirm)
}

func TestUnavailableActorModelCapabilityKeepsNotice(t *testing.T) {
	m, handle := sessionModel(t)
	handle.models = nil

	m.handleModelCommand(t.Context(), "other/model")

	assert.Empty(t, handle.modelRef)
	assert.Equal(t, 1, m.screen.Transcript.BlockCount())
}

func TestLeanSessionEditingCapabilityHidesStarMutation(t *testing.T) {
	m, handle := sessionModel(t)
	handle.sessionEditing = false

	m.handleStarSession(t.Context(), "")

	assert.Zero(t, handle.starCalls)
	assert.False(t, handle.snapshot.Starred)
	assert.Equal(t, 1, m.screen.Transcript.BlockCount())
}

func TestLeanThinkingCapabilityHidesMutation(t *testing.T) {
	m, handle := sessionModel(t)
	handle.thinkingLevels = false

	m.handleSetThinkingLevel(t.Context(), "high")

	assert.Empty(t, handle.level)
	assert.Equal(t, 1, m.screen.Transcript.BlockCount())
}

func TestLeanCompactUsesActorCapability(t *testing.T) {
	m, handle := sessionModel(t)
	m.lifecycle.Status = runtime.SessionStateSettled

	m.startCompact(t.Context(), "focus")

	assert.Equal(t, []string{"focus"}, handle.compactPrompts)
	assert.Equal(t, 0, m.screen.Transcript.BlockCount())
	assert.False(t, m.busy(), "canonical events own busy-state changes")
}

func TestLeanBoundaryCompactionDoesNotFinishActiveStream(t *testing.T) {
	m, _ := sessionModel(t)
	m.lifecycle.Status = runtime.SessionStateSettled
	m.queue = []ui.PendingUserMessage{{Display: "later", Content: "later"}}

	m.handleEvent(t.Context(), runtime.StreamStarted("root", "agent"))
	m.handleEvent(t.Context(), &runtime.SessionCompactionEvent{SessionID: "root", Status: "started"})
	m.handleEvent(t.Context(), runtime.SessionCompactionCompleted("root", runtime.CompactionOutcomeApplied, "agent"))

	assert.True(t, m.busy())
	assert.False(t, m.status.Compacting)
	assert.Equal(t, 1, m.lifecycle.Depth())
	require.Len(t, m.queue, 1, "boundary compaction must not advance queued input")

	m.handleEvent(t.Context(), runtime.StreamStopped("root", "agent", "normal"))
	assert.Equal(t, 0, m.lifecycle.Depth())
	assert.Empty(t, m.queue)
	assert.False(t, m.busy(), "submitting the queued input cannot invent a running phase")
	require.Len(t, m.pendingUsers, 1)
	m.handleEvent(t.Context(), runtime.StreamStarted("root", "agent"))
	assert.True(t, m.busy(), "canonical StreamStarted owns the next running phase")
}

func TestUnsupportedMutableExecutionCommandsAreVisible(t *testing.T) {
	t.Parallel()
	m, handle := sessionModel(t)

	m.handleSetThinkingLevel(t.Context(), "high")
	m.startSkillFork(t.Context(), "skill", "task")

	assert.Equal(t, effort.High, handle.level)
	assert.Equal(t, "high", m.status.Thinking)
	assert.Equal(t, 2, m.screen.Transcript.BlockCount())
	assert.False(t, m.busy())
}

func TestLeanLoadSessionTranscriptRestoresToolCalls(t *testing.T) {
	t.Parallel()
	sess := session.New()
	sess.AddMessage(session.UserMessage("inspect the repository"))
	sess.AddMessage(session.NewAgentMessage("coder", &chat.Message{
		Role:             chat.MessageRoleAssistant,
		ReasoningContent: "I should inspect the files.",
		Content:          "I found the issue.",
		ToolCalls: []tools.ToolCall{{
			ID:       "call-1",
			Function: tools.FunctionCall{Name: "inspect_files", Arguments: `{"path":"pkg/leantui"}`},
		}},
		ToolDefinitions: []tools.Tool{{Name: "inspect_files"}},
	}))
	sess.AddMessage(session.NewAgentMessage("coder", &chat.Message{
		Role: chat.MessageRoleTool, ToolCallID: "call-1", Content: "update.go\nview.go",
	}))

	m := bareModel(80)
	m.sessionState = service.NewSessionState(sess)
	m.loadSessionTranscript(sess)

	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "inspect the repository")
	assert.Contains(t, transcript, "I should inspect the files.")
	assert.Contains(t, transcript, "I found the issue.")
	assert.Contains(t, transcript, "inspect_files")
	assert.Contains(t, transcript, "pkg/leantui")
	assert.Contains(t, transcript, "update.go")
	assert.Zero(t, m.screen.Transcript.ToolCount())
}

func TestLeanEscapeCancelsActorAndOrdersMarkerAfterBufferedResponse(t *testing.T) {
	m, handle := cancellationModel(t)
	m.lifecycle.Status = runtime.SessionStateRunning

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	require.Equal(t, 1, handle.stops)
	assert.Equal(t, handle.turnID, handle.cancelledTurn)
	assert.True(t, m.cancelMarkerPending)
	assert.Equal(t, 0, m.screen.Transcript.BlockCount())

	m.handleEvent(t.Context(), runtime.AgentChoice("agent", handle.id, "buffered response"))
	m.handleEvent(t.Context(), runtime.StreamStopped(handle.id, "agent", "cancelled"))
	lines := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Less(t, strings.Index(lines, "buffered response"), strings.Index(lines, "Cancelled"))
	assert.False(t, m.cancelMarkerPending)
}

func TestLeanEscapeRejectsConfirmationWithoutCancellingIdleActor(t *testing.T) {
	m, handle := sessionModel(t)
	m.lifecycle.Status = runtime.SessionStateSettled
	m.screen.Confirm = &ui.ConfirmModel{Tool: "shell", SessionID: handle.id, RequestID: "request-escape"}

	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})

	require.Len(t, handle.responses, 1)
	assert.Equal(t, runtime.ResumeTypeReject, handle.responses[0].Resume.Type)
	assert.Equal(t, 0, handle.stops)
	assert.Nil(t, m.screen.Confirm)
}

func (a *leanSession) Release(context.Context) error { return nil }

func (a *leanSession) UpdateTitle(context.Context, string) error { return nil }

func TestLoadInitialSessionTranscriptMarksRestoredUserMessages(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sess.AddMessage(session.UserMessage("restored question"))
	sess.AddMessage(session.NewAgentMessage("coder", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "restored answer",
	}))

	m := bareModel(80)
	m.app = app.New(t.Context(), nil, sess, runtime.SessionBinding{})
	m.sessionState = service.NewSessionState(sess)
	m.loadInitialSessionTranscript()

	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "\x1b]133;A;redraw=0\x07")
	assert.Contains(t, transcript, "restored question")
	assert.Contains(t, transcript, "restored answer")
}

func TestCopyCommandCopiesLastAssistantResponse(t *testing.T) {
	sess := session.New()
	sess.AddMessage(session.UserMessage("question"))
	sess.AddMessage(session.NewAgentMessage("coder", &chat.Message{
		Role:    chat.MessageRoleAssistant,
		Content: "the last response",
	}))
	m := bareModel(80)
	m.app = app.New(t.Context(), nil, sess, runtime.SessionBinding{})

	originalWriteClipboard := writeClipboard
	t.Cleanup(func() { writeClipboard = originalWriteClipboard })
	var copied string
	writeClipboard = func(text string) error {
		copied = text
		return nil
	}

	assert.True(t, m.handleSlash(t.Context(), "/copy", busySubmitSteer))
	assert.Equal(t, "the last response", copied)
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "Last response copied to clipboard.")
}

func TestCopyCommandReportsMissingAssistantResponse(t *testing.T) {
	m := bareModel(80)
	m.app = app.New(t.Context(), nil, session.New(), runtime.SessionBinding{})

	originalWriteClipboard := writeClipboard
	t.Cleanup(func() { writeClipboard = originalWriteClipboard })
	called := false
	writeClipboard = func(string) error {
		called = true
		return nil
	}

	assert.True(t, m.handleSlash(t.Context(), "/copy", busySubmitSteer))
	assert.False(t, called)
	transcript := strings.Join(m.screen.Transcript.Lines(80, 0, false, m.sessionState, nil), "\n")
	assert.Contains(t, transcript, "No assistant response to copy.")
}

func TestOptionUpRestoresOnlyWithdrawnCanonicalInputs(t *testing.T) {
	t.Parallel()
	m, handle := sessionModel(t)
	m.lifecycle.Status = runtime.SessionStateRunning
	m.pendingUsers = []ui.PendingUserMessage{
		{TurnID: "consumed", Display: "already sent", Kind: ui.PendingUserSteer},
		{TurnID: "steer", Display: "first steer", Kind: ui.PendingUserSteer},
		{TurnID: "follow", Display: "then follow up", Kind: ui.PendingUserFollowUp},
	}
	handle.cancelable = map[string]bool{"steer": true, "follow": true}
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyAltUp})
	assert.Empty(t, handle.cancelable)
	require.Len(t, m.pendingUsers, 1)
	assert.Equal(t, "consumed", m.pendingUsers[0].TurnID)
	assert.Equal(t, "first steer\nthen follow up", m.screen.Editor.Text())
	assert.True(t, m.busy())
	assert.Zero(t, handle.stops, "recall must not cancel the active turn")
}

func TestOptionUpWithdrawalErrorKeepsPendingInput(t *testing.T) {
	t.Parallel()
	m, handle := sessionModel(t)
	handle.withdrawErr = runtime.ErrUnsupported
	m.pendingUsers = []ui.PendingUserMessage{{TurnID: "pending", Display: "keep me", Kind: ui.PendingUserSteer}}
	m.screen.Editor.SetText("draft")
	assert.False(t, m.cancelPendingMessages(t.Context()))
	assert.Len(t, m.pendingUsers, 1)
	assert.Equal(t, "draft", m.screen.Editor.Text())
}

func TestProjectedReasoningWaitsForCanonicalRefreshAfterCycle(t *testing.T) {
	m, handle := sessionModel(t)
	m.status.ThinkingMode = "effort"
	m.status.ThinkingLevel = "low"
	m.status.Thinking = "low"
	m.status.CanCycleThinking = true
	m.handleCycleThinkingLevel(t.Context())
	assert.Equal(t, effort.High, handle.level)
	assert.Equal(t, "low", m.status.ThinkingLevel, "action must not infer projection before the canonical event")
	m.handleEvent(t.Context(), runtime.TeamInfo([]runtime.AgentDetails{{Name: "agent", ThinkingMode: "effort", ThinkingLevel: "high", CanCycleThinking: true}}, "agent"))
	assert.Equal(t, "high", m.status.ThinkingLevel)
}

func TestFriendlyModelCompletionRetainsCanonicalRef(t *testing.T) {
	m, handle := sessionModel(t)
	handle.models = []runtime.ModelChoice{{Name: "config-alias", Ref: "config-alias", Provider: "provider", ModelID: "canonical-id", ModelName: "Friendly Model"}}
	m.handleModelCommand(t.Context(), "")
	choice, ok := m.screen.Autocomplete.Current()
	require.True(t, ok)
	assert.Equal(t, "Friendly Model", choice.Name)
	assert.Equal(t, "config-alias", choice.Value)
}

func TestShiftTabUsesPrimaryCapabilityAcrossFallbackProjectionTransitions(t *testing.T) {
	for _, capable := range []bool{true, false} {
		m, handle := sessionModel(t)
		handle.thinkingLevels = capable
		primary := &runtime.ThinkingDetails{ModelRef: "p/primary", Mode: "effort", Level: "low", CanCycle: capable}
		if !capable {
			primary.Mode, primary.Level = "unsupported", "unsupported"
		}
		for _, fallback := range []bool{false, true, false} {
			modelRef := "primary"
			if fallback {
				modelRef = "fallback"
			}
			m.handleEvent(t.Context(), runtime.AgentInfo("agent", "p/"+modelRef, "", ""))
			// The gap before TeamInfo must use the same handle capability.
			handle.level = ""
			m.handleKey(t.Context(), ui.Key{Typ: ui.KeyShiftTab})
			if capable {
				assert.Equal(t, effort.High, handle.level)
			} else {
				assert.Empty(t, handle.level)
			}
			details := runtime.AgentDetails{
				Name: "agent", Provider: "p", ModelID: modelRef, ModelName: "Friendly " + modelRef,
				ThinkingMode: primary.Mode, ThinkingLevel: primary.Level, CanCycleThinking: capable,
			}
			if fallback {
				details.PrimaryThinking = primary
				details.ThinkingMode, details.ThinkingLevel = "effort", "high"
				if capable {
					details.ThinkingMode, details.ThinkingLevel = "unsupported", "unsupported"
				}
				details.CanCycleThinking = !capable
			}
			m.handleEvent(t.Context(), runtime.TeamInfo([]runtime.AgentDetails{details}, "agent"))
			handle.level = ""
			m.handleKey(t.Context(), ui.Key{Typ: ui.KeyShiftTab})
			if capable {
				assert.Equal(t, effort.High, handle.level)
			} else {
				assert.Empty(t, handle.level)
			}
			assert.Equal(t, details.ThinkingLevel, m.status.ThinkingLevel, "keyboard action cannot rewrite F projection")
			if fallback {
				lines := strings.Join(ui.RenderStatus(m.status, 120), "\n")
				assert.Contains(t, lines, "Friendly fallback")
				assert.Contains(t, lines, "p("+details.ThinkingLevel+")")
				assert.Contains(t, lines, "(primary: "+primary.Level+")")
			}
		}
		m.handleEvent(t.Context(), runtime.AgentInfo("other-agent", m.status.Model, "", ""))
		assert.Empty(t, m.status.ThinkingMode, "agent replacement invalidates reasoning even if model identity is unchanged")
		assert.Nil(t, m.status.PrimaryThinking)
	}
}

func TestSkillCompletionCannotAdvanceQueueTwice(t *testing.T) {
	m, handle := sessionModel(t)
	handle.submitted = nil
	m.ownedSkillOperation = "skill"
	m.queue = []ui.PendingUserMessage{{Content: "first"}, {Content: "second"}}
	m.handleEvent(t.Context(), runtime.StreamStarted(handle.id, "agent"))
	m.handleEvent(t.Context(), runtime.StreamStopped(handle.id, "agent", "normal"))
	require.Len(t, handle.submitted, 1)
	require.Len(t, m.queue, 1)
	m.handleEvent(t.Context(), &runtime.SkillOperationEvent{OperationID: "skill", Status: "completed"})
	assert.Empty(t, m.ownedSkillOperation)
	assert.Len(t, handle.submitted, 1, "operation completion is not another canonical stream boundary")
	assert.Len(t, m.queue, 1)
	assert.False(t, m.busy(), "skill completion cannot invent the next execution phase")
}
