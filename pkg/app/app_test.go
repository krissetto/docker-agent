package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// mockRuntime implements only App's presentation Services surface.
type mockRuntime struct {
	store session.Store
}

func (m *mockRuntime) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}

func (m *mockRuntime) CurrentAgentTools(context.Context) ([]tools.Tool, error) { return nil, nil }

func (m *mockRuntime) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }

func (m *mockRuntime) RestartToolset(context.Context, string) error                         { return nil }
func (m *mockRuntime) EmitStartupInfo(context.Context, *session.Session, runtime.EventSink) {}
func (m *mockRuntime) EmitAgentInfo(context.Context, runtime.EventSink)                     {}
func (m *mockRuntime) ResetStartupInfo()                                                    {}
func (m *mockRuntime) SessionStore() session.Store                                          { return m.store }

func (m *mockRuntime) PermissionsInfo() *runtime.PermissionsInfo { return nil }

func (m *mockRuntime) CurrentAgentSkillsToolset() *skillstool.ToolSet { return nil }

func (m *mockRuntime) CurrentMCPPrompts(context.Context) map[string]mcptools.PromptInfo {
	return map[string]mcptools.PromptInfo{}
}

func (m *mockRuntime) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", nil
}

func (m *mockRuntime) UpdateSessionTitle(_ context.Context, sess *session.Session, title string) error {
	sess.Title = title
	return nil
}
func (m *mockRuntime) OnToolsChanged(func(runtime.Event))    {}
func (m *mockRuntime) OnBackgroundEvent(func(runtime.Event)) {}

var _ Services = (*mockRuntime)(nil)

// backgroundEventMockRuntime captures the handler App.Start registers via
// OnBackgroundEvent so tests can emit background events through it.
type backgroundEventMockRuntime struct {
	mockRuntime

	handler func(runtime.Event)
}

func (m *backgroundEventMockRuntime) OnBackgroundEvent(handler func(runtime.Event)) {
	m.handler = handler
}

// TestApp_Start_ForwardsBackgroundEvents verifies Start wires the runtime's
// out-of-band background-event hook into the app's event stream, so token
// usage from background agent tasks reaches the TUI subscribers.
func TestApp_Start_ForwardsBackgroundEvents(t *testing.T) {
	t.Parallel()

	rt := &backgroundEventMockRuntime{}
	events := make(chan any, 16)
	app := &App{
		runtime:      rt,
		currentState: sessionState{session: session.New()},
		events:       events,
	}

	app.Start(t.Context())
	require.NotNil(t, rt.handler, "Start must register the background-event handler")

	usage := runtime.NewTokenUsageEvent("bg-session", "worker", &runtime.Usage{
		ContextLength: 150,
		ContextLimit:  1000,
	})
	rt.handler(usage)

	select {
	case msg := <-events:
		assert.Equal(t, usage, msg, "the background event must reach the app's event stream unchanged")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the forwarded background event")
	}
}

// TestApp_SendEvent_DeliversElicitationWithoutDedupe verifies the App bus
// remains stateless: two session observations with the same elicitation ID are
// both forwarded, leaving request correlation to SessionHandle.Respond.
func TestApp_SendEvent_DeliversElicitationWithoutDedupe(t *testing.T) {
	t.Parallel()

	events := make(chan any, 16)
	app := &App{events: events}
	ctx := t.Context()

	ev := runtime.ElicitationRequest("need input", "form", nil, "", "eid-dup", "", "sess-1", nil, "worker")
	app.sendEvent(ctx, ev)
	require.Len(t, events, 1, "the sink's single delivery must reach the app's event stream")
	<-events

	// A second event that happens to carry the same ElicitationID (e.g. a
	// canceled request's ID reused later) must still go through: nothing in
	// the App layer keys off ElicitationID any more.
	app.sendEvent(ctx, ev)
	require.Len(t, events, 1, "sendEvent must not drop a delivery based on ElicitationID")
}

// stubSnapshotController is a tiny SnapshotController used by the app
// tests to drive /undo without spinning up a real shadow-git
// repository. enabled gates SnapshotsEnabled(), and the (files, ok,
// err) tuple is returned verbatim from UndoLast / Reset so each test
// can assert the result-shaping logic in [snapshotResult].
type stubSnapshotController struct {
	enabled bool
	files   int
	ok      bool
	err     error
}

func (s *stubSnapshotController) Enabled() bool { return s.enabled }
func (s *stubSnapshotController) UndoLast(context.Context, string, string) (int, bool, error) {
	return s.files, s.ok, s.err
}

func (s *stubSnapshotController) List(string) []builtins.SnapshotInfo { return nil }
func (s *stubSnapshotController) Reset(context.Context, string, string, int) (int, bool, error) {
	return s.files, s.ok, s.err
}
func (s *stubSnapshotController) AutoInject(*hooks.Config) {}

var _ builtins.SnapshotController = (*stubSnapshotController)(nil)

func TestApp_NewSession_PreservesToolsApproved(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	// Create initial session with tools approved
	initialSess := session.New(session.WithToolsApproved(true))
	require.True(t, initialSess.ToolsApproved, "Initial session should have tools approved")

	app := New(t.Context(), nil, initialSess, runtime.SessionBinding{}, WithRuntimeServices(rt))

	// Call NewSession - should preserve ToolsApproved
	app.NewSession()

	assert.True(t, app.Session().ToolsApproved, "NewSession should preserve ToolsApproved")
}

func TestApp_NewSession_PreservesSafetyPolicy(t *testing.T) {
	t.Parallel()

	for _, policy := range []session.SafetyPolicy{session.SafetyPolicyStrict, session.SafetyPolicyBalanced} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()

			initialSess := session.New(session.WithSafetyPolicy(policy))
			app := New(t.Context(), nil, initialSess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))

			app.NewSession()

			assert.Equal(t, policy, app.Session().GetSafetyPolicy())
			assert.False(t, app.Session().ToolsApproved)
		})
	}
}

func TestApp_NewSession_PreservesPriorSafetyPolicy(t *testing.T) {
	t.Parallel()

	initialSess := session.New(session.WithSafetyPolicy(session.SafetyPolicyBalanced))
	initialSess.ToggleYolo()
	app := New(t.Context(), nil, initialSess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))

	app.NewSession()

	sess := app.Session()
	assert.Equal(t, session.SafetyPolicyAutonomous, sess.GetSafetyPolicy())
	assert.Equal(t, session.SafetyPolicyBalanced, sess.GetPriorSafetyPolicy())
	sess.ToggleYolo()
	assert.Equal(t, session.SafetyPolicyBalanced, sess.GetSafetyPolicy())
}

func TestApp_NewSession_PreservesHideToolResults(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	// Create initial session with hide tool results
	initialSess := session.New(session.WithHideToolResults(true))
	require.True(t, initialSess.HideToolResults, "Initial session should have HideToolResults")

	app := New(t.Context(), nil, initialSess, runtime.SessionBinding{}, WithRuntimeServices(rt))

	// Call NewSession - should preserve HideToolResults
	app.NewSession()

	assert.True(t, app.Session().HideToolResults, "NewSession should preserve HideToolResults")
}

func TestApp_NewSession_WithNilSession(t *testing.T) {
	t.Parallel()

	rt := &mockRuntime{}

	// Create app with nil session (edge case)
	app := &App{
		ctx:          t.Context,
		runtime:      rt,
		currentState: sessionState{session: nil},
	}

	// Call NewSession - should not panic and create a new session with defaults
	app.NewSession()

	require.NotNil(t, app.Session(), "NewSession should create a new session")
	assert.False(t, app.Session().ToolsApproved, "NewSession with nil should use default ToolsApproved=false")
}

type titleSession struct {
	projectionSession

	sess *session.Session
}

func (a *titleSession) UpdateTitle(_ context.Context, title string) error {
	a.sess.SetTitle(title)
	return nil
}

func TestApp_UpdateSessionTitle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("updates title in session", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan any, 16)
		handle := &titleSession{sess: sess}
		app := &App{
			runtime:      rt,
			currentState: sessionState{session: sess, handle: handle},
			events:       events,
		}

		err := app.UpdateSessionTitle(ctx, "New Title")
		require.NoError(t, err)

		assert.Equal(t, "New Title", sess.Title)
		assert.Empty(t, events, "title delivery comes from session observation, not direct app publication")
	})

	t.Run("returns error when no session", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		app := &App{
			runtime:      rt,
			currentState: sessionState{session: nil},
		}

		err := app.UpdateSessionTitle(ctx, "New Title")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no active session")
	})

	t.Run("returns ErrTitleGenerating when generation in progress", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan any, 16)
		app := &App{
			runtime:      rt,
			currentState: sessionState{session: sess},
			events:       events,
		}

		// Simulate title generation in progress
		app.titleGenerating.Store(true)

		err := app.UpdateSessionTitle(ctx, "New Title")
		require.ErrorIs(t, err, ErrTitleGenerating)

		// Title should not be updated
		assert.Empty(t, sess.Title)
	})
}

func TestApp_ResolveSkillCommand_NoLocalRuntime(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}
	sess := session.New()
	app := New(t.Context(), nil, sess, runtime.SessionBinding{}, WithRuntimeServices(rt))

	// mockRuntime is not a LocalRuntime, so no skills should be returned
	resolved, err := app.ResolveSkillCommand(ctx, "/some-skill")
	require.NoError(t, err)
	assert.Empty(t, resolved)
}

func TestApp_ResolveSkillCommand_NotSlashCommand(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	rt := &mockRuntime{}
	sess := session.New()
	app := New(t.Context(), nil, sess, runtime.SessionBinding{}, WithRuntimeServices(rt))

	resolved, err := app.ResolveSkillCommand(ctx, "not a slash command")
	require.NoError(t, err)
	assert.Empty(t, resolved)
}

func TestApp_UndoLastSnapshot(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}),
		WithSnapshotController(&stubSnapshotController{enabled: true, files: 2, ok: true}),
	)
	result, err := app.UndoLastSnapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, result.RestoredFiles)
}

func TestApp_UndoLastSnapshot_NoSnapshot(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}),
		WithSnapshotController(&stubSnapshotController{enabled: true}),
	)
	_, err := app.UndoLastSnapshot(ctx)
	assert.ErrorIs(t, err, ErrNothingToUndo)
}

func TestApp_UndoLastSnapshot_NoController(t *testing.T) {
	t.Parallel()

	// Without a SnapshotController the App reports nothing to undo,
	// so the same UI affordance can light up regardless of which
	// runtime the embedder paired the App with.
	ctx := t.Context()
	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	_, err := app.UndoLastSnapshot(ctx)
	require.ErrorIs(t, err, ErrNothingToUndo)
	assert.False(t, app.SnapshotsEnabled())
}

func TestApp_SnapshotsEnabled_DoesNotRequireSession(t *testing.T) {
	t.Parallel()

	// SnapshotsEnabled answers a controller-capability question; it
	// must not silently return false just because no session is attached.
	app := &App{
		ctx:                t.Context,
		runtime:            &mockRuntime{},
		currentState:       sessionState{session: nil},
		snapshotController: &stubSnapshotController{enabled: true},
	}
	assert.True(t, app.SnapshotsEnabled())
}

func TestApp_SubscribeWith_FanOutToMultipleSubscribers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rt := &mockRuntime{}
	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(rt))

	recv := func() (chan any, context.CancelFunc) {
		subCtx, subCancel := context.WithCancel(ctx)
		ch := make(chan any, 16)
		go app.SubscribeWith(subCtx, func(m tea.Msg) { ch <- m })
		return ch, subCancel
	}

	a, cancelA := recv()
	b, cancelB := recv()
	defer cancelA()
	defer cancelB()

	// Wait until both subscribers are registered before publishing.
	require.Eventually(t, func() bool {
		app.subsMu.Lock()
		defer app.subsMu.Unlock()
		return len(app.subs) == 2
	}, time.Second, 5*time.Millisecond)

	app.events <- runtime.SessionTitle("sess", "hello")

	for _, ch := range []chan any{a, b} {
		select {
		case msg := <-ch:
			ev, ok := msg.(*runtime.SessionTitleEvent)
			require.True(t, ok)
			assert.Equal(t, "hello", ev.Title)
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive event")
		}
	}
}

func TestApp_RegenerateSessionTitle(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	t.Run("returns error when no session", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		app := &App{
			runtime:      rt,
			currentState: sessionState{session: nil},
		}

		err := app.RegenerateSessionTitle(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no active session")
	})

	t.Run("returns error when no title generator is available", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan any, 16)
		app := &App{
			runtime:      rt,
			currentState: sessionState{session: sess},
			events:       events,
			// titleGen is nil - no title generator available
		}

		err := app.RegenerateSessionTitle(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "title regeneration not available")
	})

	t.Run("returns ErrTitleGenerating when already generating", func(t *testing.T) {
		t.Parallel()

		rt := &mockRuntime{}
		sess := session.New()
		events := make(chan any, 16)
		app := &App{
			runtime:      rt,
			currentState: sessionState{session: sess},
			events:       events,
		}

		// Simulate title generation already in progress
		app.titleGenerating.Store(true)

		err := app.RegenerateSessionTitle(ctx)
		require.ErrorIs(t, err, ErrTitleGenerating)
	})
}

func TestApp_DropAttachedFile(t *testing.T) {
	t.Parallel()

	newAppWithAttachments := func(store session.Store, paths ...string) (*App, *session.Session) {
		sess := session.New(session.WithAttachedFiles(paths))
		return New(t.Context(), nil, sess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{store: store})), sess
	}

	t.Run("drops by exact path and syncs the store", func(t *testing.T) {
		t.Parallel()
		store := session.NewInMemorySessionStore()
		app, sess := newAppWithAttachments(store, "/abs/foo.go", "/abs/bar.go")

		dropped, err := app.DropAttachedFile(t.Context(), "/abs/foo.go")
		require.NoError(t, err)
		assert.Equal(t, "/abs/foo.go", dropped)
		assert.Equal(t, []string{"/abs/bar.go"}, sess.AttachedFilesSnapshot())

		stored, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{"/abs/bar.go"}, stored.AttachedFilesSnapshot())
	})

	t.Run("drops by unique base name", func(t *testing.T) {
		t.Parallel()
		app, sess := newAppWithAttachments(nil, "/abs/dir/foo.go", "/abs/dir/bar.go")

		dropped, err := app.DropAttachedFile(t.Context(), "foo.go")
		require.NoError(t, err)
		assert.Equal(t, "/abs/dir/foo.go", dropped)
		assert.Equal(t, []string{"/abs/dir/bar.go"}, sess.AttachedFilesSnapshot())
	})

	t.Run("rejects ambiguous base names", func(t *testing.T) {
		t.Parallel()
		app, sess := newAppWithAttachments(nil, "/abs/a/foo.go", "/abs/b/foo.go")

		_, err := app.DropAttachedFile(t.Context(), "foo.go")
		require.ErrorContains(t, err, "matches 2 attached files")
		assert.Len(t, sess.AttachedFilesSnapshot(), 2)
	})

	t.Run("rejects unknown files and blank input", func(t *testing.T) {
		t.Parallel()
		app, _ := newAppWithAttachments(nil, "/abs/foo.go")

		_, err := app.DropAttachedFile(t.Context(), "/abs/other.go")
		require.ErrorContains(t, err, "not attached")

		_, err = app.DropAttachedFile(t.Context(), "   ")
		require.ErrorContains(t, err, "no file specified")
	})

	t.Run("reports when nothing is attached", func(t *testing.T) {
		t.Parallel()
		app, _ := newAppWithAttachments(nil)

		_, err := app.DropAttachedFile(t.Context(), "foo.go")
		require.ErrorContains(t, err, "no files are attached")
	})

	t.Run("returns error when no session", func(t *testing.T) {
		t.Parallel()
		app := &App{runtime: &mockRuntime{}}

		_, err := app.DropAttachedFile(t.Context(), "foo.go")
		require.ErrorContains(t, err, "no active session")
	})
}

func TestResolveAttachedFile_RelativePath(t *testing.T) {
	t.Parallel()

	abs, err := filepath.Abs("notes.md")
	require.NoError(t, err)

	resolved, err := resolveAttachedFile([]string{abs}, "notes.md")
	require.NoError(t, err)
	assert.Equal(t, abs, resolved)
}

// liveSessionsMockRuntime layers the optional live-session capabilities
// (LiveSessions, CompactLiveSession) over the base mock runtime.
type liveSessionsMockRuntime struct {
	mockRuntime

	rows        []runtime.LiveSession
	lastCurrent *session.Session
	compactedID string
	compactErr  error
}

func (m *liveSessionsMockRuntime) LiveSessions(_ context.Context, current *session.Session) []runtime.LiveSession {
	m.lastCurrent = current
	return m.rows
}

func (m *liveSessionsMockRuntime) CompactLiveSession(_ context.Context, sessionID, _ string, events runtime.EventSink) error {
	if m.compactErr != nil {
		return m.compactErr
	}
	m.compactedID = sessionID
	events.Emit(runtime.SessionCompactionCompleted(sessionID, runtime.CompactionOutcomeApplied, "worker"))
	return nil
}

// thinkingLevelsMockRuntime is a minimal mockRuntime extension implementing
// agentThinkingLevelsProvider, exercising the pass-through half of
// App.CurrentAgentThinkingLevels (the type-assertion-miss half is covered by
// plain *mockRuntime, which does not implement the interface).
type thinkingLevelsMockRuntime struct {
	mockRuntime

	levels []effort.Level
}

func (m *thinkingLevelsMockRuntime) CurrentAgentThinkingLevels(context.Context) []effort.Level {
	return m.levels
}

func TestApp_CurrentAgentThinkingLevels_UnsupportedRuntime(t *testing.T) {
	t.Parallel()

	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	assert.Nil(t, app.CurrentAgentThinkingLevels(t.Context()),
		"runtimes without a thinking-levels resolution degrade to no /effort candidates")
}

func TestApp_CurrentAgentThinkingLevels_PassesThroughRuntimeLevels(t *testing.T) {
	t.Parallel()

	rt := &thinkingLevelsMockRuntime{levels: []effort.Level{effort.Low, effort.Medium, effort.High}}
	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(rt))

	assert.Equal(t, []effort.Level{effort.Low, effort.Medium, effort.High}, app.CurrentAgentThinkingLevels(t.Context()))
}

func TestApp_LiveSessions_UnsupportedRuntime(t *testing.T) {
	t.Parallel()

	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	assert.Nil(t, app.LiveSessions(t.Context()),
		"runtimes without live-session tracking degrade to an empty team view")
}

func TestApp_CompactLiveSession_UnsupportedRuntime(t *testing.T) {
	t.Parallel()

	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
	err := app.CompactLiveSession(t.Context(), "some-session", "")
	require.ErrorIs(t, err, runtime.ErrUnsupported)
}

func TestApp_LiveSessions_PassesCurrentSession(t *testing.T) {
	t.Parallel()

	sess := session.New()
	rt := &liveSessionsMockRuntime{rows: []runtime.LiveSession{
		{SessionID: sess.ID, AgentName: "root", Current: true},
		{SessionID: "child-1", AgentName: "worker"},
	}}
	app := New(t.Context(), nil, sess, runtime.SessionBinding{}, WithRuntimeServices(rt))

	rows := app.LiveSessions(t.Context())
	require.Len(t, rows, 2)
	assert.Same(t, sess, rt.lastCurrent, "the app's current session drives the root row")
}

func TestApp_CompactLiveSession_BridgesEventsIntoStream(t *testing.T) {
	t.Parallel()

	rt := &liveSessionsMockRuntime{}
	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(rt))

	require.NoError(t, app.CompactLiveSession(t.Context(), "child-1", ""))
	assert.Equal(t, "child-1", rt.compactedID)

	select {
	case msg := <-app.events:
		evt, ok := msg.(*runtime.SessionCompactionEvent)
		require.True(t, ok, "expected SessionCompactionEvent, got %T", msg)
		assert.Equal(t, "child-1", evt.SessionID)
		assert.Equal(t, "completed", evt.Status)
	case <-time.After(time.Second):
		t.Fatal("compaction event was not bridged into the app event stream")
	}
}

func TestApp_CompactLiveSession_ForwardsRuntimeError(t *testing.T) {
	t.Parallel()

	rt := &liveSessionsMockRuntime{compactErr: errors.New("session x is not live")}
	app := New(t.Context(), nil, session.New(), runtime.SessionBinding{}, WithRuntimeServices(rt))

	err := app.CompactLiveSession(t.Context(), "x", "")
	require.ErrorContains(t, err, "not live")
}
