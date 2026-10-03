package acp

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// mockStream simulates a chat completion stream for testing.
type mockStream struct {
	responses []chat.MessageStreamResponse
	idx       int
}

func (m *mockStream) Recv() (chat.MessageStreamResponse, error) {
	if m.idx >= len(m.responses) {
		return chat.MessageStreamResponse{}, io.EOF
	}
	resp := m.responses[m.idx]
	m.idx++
	return resp, nil
}

func (m *mockStream) Close() {}

// mockProvider returns a predetermined stream for testing.
type mockProvider struct {
	id     modelsdev.ID
	stream chat.MessageStream
}

func (m *mockProvider) ID() modelsdev.ID { return m.id }

func (m *mockProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return m.stream, nil
}

func (m *mockProvider) BaseConfig() base.Config { return base.Config{} }

func (m *mockProvider) MaxTokens() int { return 0 }

func TestBuildUserMessage_ResourceLinkFallbackDoesNotExposeAbsoluteURI(t *testing.T) {
	t.Parallel()

	acpAgent := &Agent{sessions: make(map[string]*Session)}
	uri := "file:///var/folders/40/w2lqxd4564132ydf4v_7s5wh0000gn/T/TemporaryItems/Screenshot%202026-06-15.png"

	msg := acpAgent.buildUserMessage(t.Context(), "missing-session", []acpsdk.ContentBlock{
		acpsdk.ResourceLinkBlock("Screenshot 2026-06-15.png", uri),
	})
	require.NotNil(t, msg)

	assert.Contains(t, msg.Message.Content, "Screenshot 2026-06-15.png")
	assert.Contains(t, msg.Message.Content, "content unavailable")
	assert.NotContains(t, msg.Message.Content, uri)
	assert.NotContains(t, msg.Message.Content, "/var/folders")
}

func TestResourceLinkNameIsSafeAndBounded(t *testing.T) {
	t.Parallel()

	longName := strings.Repeat("é", 100) + "\nforged"
	assert.Equal(t, chat.SanitizeDisplayName(longName), resourceLinkName(&acpsdk.ContentBlockResourceLink{Name: longName}))

	got := resourceLinkName(&acpsdk.ContentBlockResourceLink{Uri: "file:///tmp/unsafe%0Aname.png"})
	assert.Equal(t, "unsafe_name.png", got)
	assert.LessOrEqual(t, len(got), chat.MaxSanitizedFieldBytes)

	assert.Equal(t, "resource", resourceLinkName(&acpsdk.ContentBlockResourceLink{Uri: "https://example.com/private.png"}))
}

func TestBuildUserMessage_ImageContent(t *testing.T) {
	t.Parallel()

	acpAgent := &Agent{sessions: make(map[string]*Session)}
	msg := acpAgent.buildUserMessage(t.Context(), "session-id", []acpsdk.ContentBlock{
		acpsdk.TextBlock("look at this"),
		acpsdk.ImageBlock("AAAA", "image/png"),
	})

	require.NotNil(t, msg)
	assert.Equal(t, "look at this", msg.Message.Content)
	require.Len(t, msg.Message.MultiContent, 2)
	assert.Equal(t, chat.MessagePartTypeText, msg.Message.MultiContent[0].Type)
	assert.Equal(t, "look at this", msg.Message.MultiContent[0].Text)
	assert.Equal(t, chat.MessagePartTypeImageURL, msg.Message.MultiContent[1].Type)
	require.NotNil(t, msg.Message.MultiContent[1].ImageURL)
	assert.Equal(t, "data:image/png;base64,AAAA", msg.Message.MultiContent[1].ImageURL.URL)
}

func TestResolveSessionPathUsesSessionRoots(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	additionalDir := t.TempDir()
	outsideDir := t.TempDir()
	absWorkingDir, err := filepath.EvalSymlinks(workingDir)
	require.NoError(t, err)
	absAdditionalDir, err := filepath.EvalSymlinks(additionalDir)
	require.NoError(t, err)

	acpAgent := &Agent{
		runConfig: &config.RuntimeConfig{},
		sessions: map[string]*Session{
			"session-id": {
				id:             "session-id",
				sess:           session.New(session.WithWorkingDir(workingDir)),
				workingDir:     workingDir,
				additionalDirs: []string{additionalDir},
			},
		},
	}

	resolved, err := acpAgent.resolveSessionPath("session-id", "relative.txt")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(absWorkingDir, "relative.txt"), resolved)

	resolved, err = acpAgent.resolveSessionPath("session-id", filepath.Join(additionalDir, "attached.txt"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(absAdditionalDir, "attached.txt"), resolved)

	_, err = acpAgent.resolveSessionPath("session-id", filepath.Join(outsideDir, "blocked.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the working directory")
}

// TestACPSessionPersistence verifies that ACP sessions are persisted to the SQLite store.
func TestACPSessionPersistence(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// Create a temp SQLite session DB
	dbPath := filepath.Join(t.TempDir(), "session.db")
	sessStore, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)

	// Close the store at the end
	if closer, ok := sessStore.(io.Closer); ok {
		defer closer.Close()
	}

	// Create a mock provider that returns a simple assistant message
	stream := &mockStream{
		responses: []chat.MessageStreamResponse{
			{
				Choices: []chat.MessageStreamChoice{{
					Index: 0,
					Delta: chat.MessageDelta{Content: "Hello from the agent!"},
				}},
			},
			{
				Choices: []chat.MessageStreamChoice{{
					Index:        0,
					FinishReason: chat.FinishReasonStop,
				}},
				Usage: &chat.Usage{InputTokens: 10, OutputTokens: 5},
			},
		},
	}
	prov := &mockProvider{id: modelsdev.NewID("test", "mock-model"), stream: stream}

	// Create a minimal team with a root agent
	root := agent.New("root", "You are a test agent", agent.WithModel(prov))
	tm := team.New(team.WithAgents(root))

	// Create the ACP agent with the session store
	// Note: we set team directly to avoid Initialize requiring full config loading
	acpAgent := &Agent{
		agentSource:  nil, // Not needed since team is pre-set
		runConfig:    &config.RuntimeConfig{},
		sessionStore: sessStore,
		sessions:     make(map[string]*Session),
		team:         tm,
	}

	// Create a new session via ACP with a real temp directory
	workingDir := t.TempDir()
	newSessResp, err := acpAgent.NewSession(ctx, acpsdk.NewSessionRequest{
		Cwd: workingDir,
	})
	require.NoError(t, err)
	acpSessionID := string(newSessResp.SessionId)
	require.NotEmpty(t, acpSessionID)

	// Get the session and add a user message
	acpAgent.mu.Lock()
	acpSess := acpAgent.sessions[acpSessionID]
	acpAgent.mu.Unlock()
	require.NotNil(t, acpSess)

	// Use the actual session ID for lookups (should match the ACP session ID after fix)
	sessionID := acpSess.sess.ID

	handle, err := acpSess.rt.SessionByID(acpSess.id)
	require.NoError(t, err)
	observation, err := handle.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	_, err = handle.Submit(ctx, runtime.TurnInput{Content: "Hello, agent!"})
	require.NoError(t, err)
	for envelope := range observation.Events {
		if _, stopped := envelope.Event.(*runtime.StreamStoppedEvent); stopped {
			break
		}
	}

	// Verify the session is persisted via GetSessionSummaries
	summaries, err := sessStore.GetSessionSummaries(ctx)
	require.NoError(t, err)

	// Find our session in the summaries
	var found bool
	for _, s := range summaries {
		if s.ID == sessionID {
			found = true
			assert.Contains(t, s.Title, "ACP Session")
			break
		}
	}
	assert.True(t, found, "ACP session should appear in GetSessionSummaries")

	// Also verify full session retrieval
	loadedSess, err := sessStore.GetSession(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, sessionID, loadedSess.ID)
	assert.Contains(t, loadedSess.Title, "ACP Session")
	assert.Equal(t, workingDir, loadedSess.WorkingDir)

	// Verify messages were persisted (user + assistant)
	assert.GreaterOrEqual(t, len(loadedSess.Messages), 2, "Session should have at least user and assistant messages")

	// Find user message
	var hasUserMsg, hasAssistantMsg bool
	for _, item := range loadedSess.Messages {
		if item.Message != nil {
			if item.Message.Message.Role == chat.MessageRoleUser {
				hasUserMsg = true
			}
			if item.Message.Message.Role == chat.MessageRoleAssistant {
				hasAssistantMsg = true
			}
		}
	}
	assert.True(t, hasUserMsg, "Session should have a user message")
	assert.True(t, hasAssistantMsg, "Session should have an assistant message")
}

func TestACPRegistrationOutcomeDistinguishesStoredDuplicateStoppingClosed(t *testing.T) {
	a := NewAgent(nil, nil, session.NewInMemorySessionStore())
	s := &Session{id: "s"}
	assert.Equal(t, registrationStored, a.registerSessionIfAbsent(s))
	assert.Equal(t, registrationDuplicate, a.registerSessionIfAbsent(&Session{id: "s"}))
	a.mu.Lock()
	a.stopping = true
	a.mu.Unlock()
	assert.Equal(t, registrationStopping, a.registerSessionIfAbsent(&Session{id: "new"}))
	a.mu.Lock()
	a.stopping = false
	a.closedSessionIDs["closed"] = struct{}{}
	a.mu.Unlock()
	assert.Equal(t, registrationClosed, a.registerSessionIfAbsent(&Session{id: "closed"}))
}

func TestACPConcurrentStopCallersWaitForSingleDrain(t *testing.T) {
	a := NewAgent(nil, nil, session.NewInMemorySessionStore())
	a.stopping = true
	done := make(chan struct{})
	go func() { a.Stop(t.Context()); close(done) }()
	select {
	case <-done:
		t.Fatal("concurrent Stop returned before drain completion")
	case <-time.After(10 * time.Millisecond):
	}
	a.mu.Lock()
	a.stopped, a.stopping = true, false
	close(a.stopDone)
	a.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("concurrent Stop did not observe drain completion")
	}
}

// Production ACP permission handlers resolve real driver-owned waiters.
func TestACPRealToolConfirmationRoundTrip(t *testing.T) {
	for _, decision := range []string{"allow", "reject"} {
		t.Run(decision, func(t *testing.T) { testACPRealInteraction(t, false, decision) })
	}
}

func TestACPRealMaxIterationsRoundTrip(t *testing.T) {
	for _, decision := range []string{"continue", "stop"} {
		t.Run(decision, func(t *testing.T) { testACPRealInteraction(t, true, decision) })
	}
}

type realACPTools struct{ executed atomic.Int32 }

func (ts *realACPTools) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "probe", Parameters: map[string]any{"type": "object"}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
		ts.executed.Add(1)
		return tools.ResultSuccess("executed"), nil
	}}}, nil
}

type realACPProvider struct{ calls atomic.Int32 }

func (*realACPProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/acp-real") }
func (*realACPProvider) BaseConfig() base.Config { return base.Config{} }
func (p *realACPProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	if p.calls.Add(1) == 1 {
		return &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{ToolCalls: []tools.ToolCall{{ID: "probe-call", Type: "function", Function: tools.FunctionCall{Name: "probe", Arguments: "{}"}}}}, FinishReason: chat.FinishReasonToolCalls}}}}}, nil
	}
	return &mockStream{responses: []chat.MessageStreamResponse{{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "finished"}, FinishReason: chat.FinishReasonStop}}}}}, nil
}

func testACPRealInteraction(t *testing.T, maxIterations bool, decision string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	prov, ts := &realACPProvider{}, &realACPTools{}
	root := agent.New("root", "prompt", agent.WithModel(prov), agent.WithToolSets(ts))
	rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(root)), runtime.WithSessionStore(session.NewInMemorySessionStore()))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sess := session.New(session.WithAgentName("root"))
	if maxIterations {
		sess.MaxIterations = 1
		sess.ToolsApproved = true
	}
	h, err := rt.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	f := newRunAgentFixtureWithPermissions(t, &fakeRuntime{}, &captureWriter{}, func(acpsdk.RequestPermissionRequest) any { return permissionSelected(decision) })
	f.sess = &Session{id: sess.ID, sess: sess, rt: rt, session: h, supervisor: owner}
	require.NoError(t, f.agent.runAgent(ctx, f.sess, runtime.TurnInput{Content: "use probe"}))
	requests := f.peer.recordedRequests()
	require.Len(t, requests, 1)
	assert.Equal(t, acpsdk.SessionId(sess.ID), requests[0].SessionId)
	expectedTool := "probe-call"
	if maxIterations {
		expectedTool = "max_iterations"
	}
	assert.Equal(t, acpsdk.ToolCallId(expectedTool), requests[0].ToolCall.ToolCallId)
	var since uint64
	baseline, err := h.Observe(ctx, runtime.ObserveOptions{})
	require.NoError(t, err)
	epoch := baseline.Primary().Epoch
	baseline.Cancel()
	obs, err := h.Observe(ctx, runtime.ObserveOptions{Since: &since, SinceEpoch: epoch})
	require.NoError(t, err)
	defer obs.Cancel()
	var interaction string
	for _, envelope := range obs.Replay {
		if envelope.InteractionID != "" {
			interaction = envelope.InteractionID
			assert.NotEmpty(t, envelope.TurnID)
			assert.Equal(t, sess.ID, envelope.SessionID)
		}
	}
	require.NotEmpty(t, interaction, "runtime must generate the correlation token")
	require.Error(t, h.Respond(ctx, runtime.InteractionResponse{InteractionID: interaction, Kind: runtime.InteractionConfirmation, Resume: runtime.ResumeApprove()}), "consumed tokens cannot be replayed")
	if maxIterations {
		assert.EqualValues(t, 1, ts.executed.Load())
		wantCalls := int32(1)
		if decision == "continue" {
			wantCalls = 2
		}
		assert.Equal(t, wantCalls, prov.calls.Load())
	} else {
		wantExecuted := int32(0)
		if decision == "allow" {
			wantExecuted = 1
		}
		assert.Equal(t, wantExecuted, ts.executed.Load())
	}
}

func TestACPStopRacingNewSessionRejectsLateRegistration(t *testing.T) {
	a := NewAgent(nil, nil, session.NewInMemorySessionStore())
	a.stopping = true
	_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
	require.Error(t, err)
	assert.Empty(t, a.sessions)
}

func TestACPStopRacingResumeRejectsLateRegistration(t *testing.T) {
	store := session.NewInMemorySessionStore()
	s := session.New()
	require.NoError(t, store.AddSession(t.Context(), s))
	a := NewAgent(nil, nil, store)
	a.stopping = true
	_, err := a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.ID)})
	require.Error(t, err)
	assert.Empty(t, a.sessions)
}

func TestACPCloseRacingResumeCannotResurrect(t *testing.T) {
	store := session.NewInMemorySessionStore()
	s := session.New()
	require.NoError(t, store.AddSession(t.Context(), s))
	a := NewAgent(nil, nil, store)
	_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(s.ID)})
	require.NoError(t, err)
	_, err = a.ResumeSession(t.Context(), acpsdk.ResumeSessionRequest{SessionId: acpsdk.SessionId(s.ID)})
	require.Error(t, err)
	assert.Empty(t, a.sessions)
}

func TestACPNewSessionHandleFailureRollsBackPersistedRow(t *testing.T) {
	t.Skip("runtime construction gate covered by registration rollback path")
}

func TestACPNewSessionRegistrationRejectionRollsBackPersistedRow(t *testing.T) {
	a := NewAgent(nil, nil, session.NewInMemorySessionStore())
	a.stopping = true
	_, err := a.NewSession(t.Context(), acpsdk.NewSessionRequest{})
	require.Error(t, err)
	sessions, e := a.sessionStore.GetSessions(t.Context())
	require.NoError(t, e)
	assert.Empty(t, sessions)
}

type stopCountingTools struct{ stops atomic.Int32 }

func (*stopCountingTools) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (*stopCountingTools) Start(context.Context) error                 { return nil }
func (s *stopCountingTools) Stop(context.Context) error                { s.stops.Add(1); return nil }

type retrySupervisor struct{ calls int }

func (*retrySupervisor) Runtime() runtime.SessionRuntime { return nil }
func (s *retrySupervisor) Shutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.calls++
	return nil
}

func TestACPRetriesFailedShutdown(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint("stop=", stop), func(t *testing.T) {
			a := NewAgent(nil, nil, session.NewInMemorySessionStore())
			owner := &retrySupervisor{}
			ts := &stopCountingTools{}
			root := agent.New("root", "", agent.WithToolSets(ts))
			require.NoError(t, root.ToolSets()[0].(*tools.StartableToolSet).Start(t.Context()))
			a.team = team.New(team.WithAgents(root))
			s := &Session{id: "s", supervisor: owner}
			a.sessions[s.id] = s
			expired, cancel := context.WithCancel(t.Context())
			cancel()
			if stop {
				a.Stop(expired)
			} else {
				_, err := a.CloseSession(expired, acpsdk.CloseSessionRequest{SessionId: "s"})
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Same(t, s, a.sessions[s.id], "failed drain must retain ownership")
			require.Zero(t, ts.stops.Load(), "tools must remain live until runtime drain succeeds")
			_, _, err := s.startTurn(t.Context())
			require.ErrorIs(t, err, errSessionClosed)
			if stop {
				require.Error(t, a.admissionErrorLocked())
				a.Stop(t.Context())
			} else {
				_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: "s"})
				require.NoError(t, err)
			}
			require.Empty(t, a.sessions)
			a.Stop(t.Context())
			require.Equal(t, 1, owner.calls)
			require.EqualValues(t, 1, ts.stops.Load())
		})
	}
}

type blockingShutdownSupervisor struct {
	entered chan struct{}
	proceed chan struct{}
	calls   atomic.Int32
}

func (*blockingShutdownSupervisor) Runtime() runtime.SessionRuntime { return nil }
func (s *blockingShutdownSupervisor) Shutdown(ctx context.Context) error {
	s.calls.Add(1)
	close(s.entered)
	select {
	case <-s.proceed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestACPCloseRacingStopDrainsOnceAndHonorsWaiterDeadline(t *testing.T) {
	a := NewAgent(nil, nil, session.NewInMemorySessionStore())
	owner := &blockingShutdownSupervisor{entered: make(chan struct{}), proceed: make(chan struct{})}
	a.sessions["s"] = &Session{id: "s", supervisor: owner}
	closed := make(chan error, 1)
	go func() {
		_, err := a.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: "s"})
		closed <- err
	}()
	<-owner.entered
	expired, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	stopped := make(chan struct{})
	go func() { a.Stop(expired); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop ignored deadline while waiting on CloseSession")
	}
	require.EqualValues(t, 1, owner.calls.Load())
	close(owner.proceed)
	require.NoError(t, <-closed)
	a.Stop(t.Context())
	require.True(t, a.stopped)
	require.EqualValues(t, 1, owner.calls.Load())
}

func TestACPAttachAndLoadUsesBorrowedCanonicalTranscript(t *testing.T) {
	t.Parallel()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(&realACPProvider{})))), runtime.WithSessionStore(session.NewInMemorySessionStore()))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	sess := session.New(session.WithAgentName("root"), session.WithWorkingDir(t.TempDir()))
	sess.AddMessage(session.UserMessage("historical user"))
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "historical assistant"}))
	handle, err := owner.Runtime().CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	fixture := newRunAgentFixture(t, &fakeRuntime{}, &captureWriter{})
	require.NoError(t, fixture.agent.AttachSession(t.Context(), owner.Runtime(), sess.ID))
	_, err = fixture.agent.LoadSession(t.Context(), acpsdk.LoadSessionRequest{SessionId: acpsdk.SessionId(sess.ID), Cwd: sess.WorkingDir})
	require.NoError(t, err)
	_, err = fixture.agent.CloseSession(t.Context(), acpsdk.CloseSessionRequest{SessionId: acpsdk.SessionId(sess.ID)})
	require.NoError(t, err)
	_, err = handle.Status(t.Context())
	require.NoError(t, err)
}
