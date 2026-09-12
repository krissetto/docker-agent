package tui

import (
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

// stubDialog is a minimal dialog.Dialog that records when its size is set.
// We rely on the fact that the dialog.Manager calls SetSize on opening so
// that re-using the same instance is observable from the layer count.
type stubDialog struct {
	dialog.BaseDialog

	id string
}

func (s *stubDialog) Init() tea.Cmd { return nil }
func (s *stubDialog) Update(tea.Msg) (layout.Model, tea.Cmd) {
	return s, nil
}
func (s *stubDialog) View() string             { return "stub:" + s.id }
func (s *stubDialog) SetSize(w, h int) tea.Cmd { return s.BaseDialog.SetSize(w, h) }
func (s *stubDialog) Position() (row, col int) { return 0, 0 }

// TestReplayPendingEvent_RestoresStashedDialog verifies that when the user
// switches back to a tab whose background dialog was on screen, the *same*
// dialog instance is re-opened (not a freshly built one). This guards
// against the regression in #2770 where typed-but-not-submitted user_prompt
// answers were lost on tab switch.
func TestReplayPendingEvent_RestoresStashedDialog(t *testing.T) {
	t.Parallel()

	const sessionID = "session-A"

	// Build a model with a single-session supervisor so ConsumePendingEvent
	// has a real runner to read from.
	m, _ := newTestModel(t)
	m.supervisor = supervisor.New(nil)
	addedID, err := m.supervisor.AddSession(
		t.Context(),
		nil,
		&session.Session{ID: sessionID},
		"/tmp",
		nil,
	)
	require.NoError(t, err)
	require.NotEmpty(t, addedID)
	m.sessionStates[sessionID] = service.NewSessionState(&session.Session{ID: sessionID})

	// Simulate the pre-conditions of a tab-switch-while-dialog-open:
	//   - the supervisor has a pending event for this tab
	//   - the appModel has stashed the live dialog instance keyed by tab.
	event := &runtime.ElicitationRequestEvent{Message: "ask the user"}
	m.supervisor.SetPendingEvent(sessionID, event)

	stashed := &stubDialog{id: "stashed"}
	m.stashedDialogs[sessionID] = stashedDialog{
		dialog: stashed,
		event:  event,
	}

	cmd := m.replayPendingEvent(sessionID)
	require.NotNil(t, cmd, "replayPendingEvent must return an Open command")

	msg := cmd()
	openMsg, ok := msg.(dialog.OpenDialogMsg)
	require.True(t, ok, "expected dialog.OpenDialogMsg, got %T", msg)

	// The crucial property: the SAME dialog instance is re-used so any
	// in-progress input survives the round trip.
	assert.Same(t, stashed, openMsg.Model,
		"stashed dialog instance must be re-opened to preserve in-progress input")
	assert.Same(t, event, openMsg.OriginatingEvent,
		"the open command must carry the matching originating event")

	// Stash entry is consumed exactly once.
	_, stillStashed := m.stashedDialogs[sessionID]
	assert.False(t, stillStashed, "stash entry must be removed after consumption")
}

// TestReplayPendingEvent_DiscardsStaleStash covers the case where the agent
// superseded the original prompt while the user was on a different tab. The
// stashed dialog no longer matches the new pending event, so it must be
// discarded and a fresh dialog built from the current event.
func TestReplayPendingEvent_DiscardsStaleStash(t *testing.T) {
	t.Parallel()

	const sessionID = "session-A"

	m, _ := newTestModel(t)
	m.supervisor = supervisor.New(nil)
	_, err := m.supervisor.AddSession(
		t.Context(),
		nil,
		&session.Session{ID: sessionID},
		"/tmp",
		nil,
	)
	require.NoError(t, err)
	m.sessionStates[sessionID] = service.NewSessionState(&session.Session{ID: sessionID})

	// Original event the user was answering when they left the tab.
	originalEvent := &runtime.ElicitationRequestEvent{Message: "first prompt"}
	stashed := &stubDialog{id: "stashed"}
	m.stashedDialogs[sessionID] = stashedDialog{
		dialog: stashed,
		event:  originalEvent,
	}

	// While the user was away the agent superseded the prompt with a new
	// elicitation. The supervisor's pending event no longer matches the
	// stashed one.
	newEvent := &runtime.ElicitationRequestEvent{Message: "replacement prompt"}
	m.supervisor.SetPendingEvent(sessionID, newEvent)

	cmd := m.replayPendingEvent(sessionID)
	require.NotNil(t, cmd)

	msg := cmd()
	openMsg, ok := msg.(dialog.OpenDialogMsg)
	require.True(t, ok)

	// A fresh dialog is built — it must NOT be the stale stashed instance.
	assert.NotSame(t, stashed, openMsg.Model,
		"stale stash must be discarded; a fresh dialog must be built")
	assert.Same(t, newEvent, openMsg.OriginatingEvent,
		"the open command must carry the *new* event")

	// The stale stash must not linger after this call.
	_, stillStashed := m.stashedDialogs[sessionID]
	assert.False(t, stillStashed, "stale stash entry must be removed")
}

// TestReplayPendingEvent_NoPendingEvent_ClearsStash verifies that when a tab
// has no pending event (e.g. the agent finished while the user was away on
// another tab), any leftover stash is cleared so a stale dialog isn't
// re-opened on a future switch.
func TestReplayPendingEvent_NoPendingEvent_ClearsStash(t *testing.T) {
	t.Parallel()

	const sessionID = "session-A"

	m, _ := newTestModel(t)
	m.supervisor = supervisor.New(nil)
	_, err := m.supervisor.AddSession(
		t.Context(),
		nil,
		&session.Session{ID: sessionID},
		"/tmp",
		nil,
	)
	require.NoError(t, err)
	m.sessionStates[sessionID] = service.NewSessionState(&session.Session{ID: sessionID})

	// Stash exists but the supervisor has no pending event (e.g. the stream
	// stopped while the user was on another tab).
	m.stashedDialogs[sessionID] = stashedDialog{
		dialog: &stubDialog{id: "orphan"},
		event:  &runtime.ElicitationRequestEvent{Message: "obsolete"},
	}

	cmd := m.replayPendingEvent(sessionID)
	assert.Nil(t, cmd, "no pending event ⇒ no command to run")
	_, stillStashed := m.stashedDialogs[sessionID]
	assert.False(t, stillStashed, "orphaned stash must be cleared")
}

// drainInOrder unwraps a tea.Cmd exactly the way bubbletea's real
// Program.execSequenceMsg does (see charm.land/bubbletea/v2 tea.go): a
// sequenceMsg (produced by tea.Sequence) is drained by executing each cmd
// ONE AT A TIME, IN ORDER, recursing into any nested sequence; anything else
// is a leaf Msg. sequenceMsg is an unexported `[]tea.Cmd`, so reflection is
// used to recognize its shape without depending on bubbletea internals by
// name — but a slice-of-Cmd shape alone is not enough to tell it apart from
// the exported tea.BatchMsg (also `[]tea.Cmd`, produced by tea.Batch and run
// CONCURRENTLY with no ordering guarantee by the real Program). Treating
// both shapes the same here would make this helper — and the FIFO-order test
// below — pass identically whether production used tea.Sequence or
// tea.Batch, silently defeating the regression test it exists for. So a
// tea.BatchMsg is rejected outright, and only a value whose concrete type is
// bubbletea's private sequenceMsg is recursed into.
func drainInOrder(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return []tea.Msg{msg}
	}
	rt := reflect.TypeOf(msg)
	if rt == reflect.TypeFor[tea.BatchMsg]() {
		t.Fatalf("drainInOrder got a tea.BatchMsg: production must batch replayed dialogs with tea.Sequence (ordered), not tea.Batch (concurrent, unordered)")
	}
	if rt.Kind() == reflect.Slice && rt.Elem() == reflect.TypeFor[tea.Cmd]() && rt.Name() == "sequenceMsg" {
		v := reflect.ValueOf(msg)
		var out []tea.Msg
		for i := range v.Len() {
			sub, _ := reflect.TypeAssert[tea.Cmd](v.Index(i))
			out = append(out, drainInOrder(t, sub)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// TestReplayPendingEvent_ReplaysConcurrentElicitationsInFIFOOrder is the
// should-fix regression test for deterministic replay ordering: two
// concurrent background-job elicitations queued on the same inactive tab
// must reopen as dialogs in the order they arrived. Before the fix,
// replayPendingEvent batched the OpenDialogMsg commands with tea.Batch,
// which bubbletea's real Program executes CONCURRENTLY with no ordering
// guarantee on which resulting Msg reaches Update first (see
// Program.execBatchMsg) — so which prompt ended up on top of the dialog
// stack was nondeterministic. This drains the actual tea.Cmd graph the way
// the real Program does (see drainInOrder) instead of only inspecting the
// internal queue slice order, so it would catch a regression back to
// tea.Batch.
func TestReplayPendingEvent_ReplaysConcurrentElicitationsInFIFOOrder(t *testing.T) {
	t.Parallel()

	const sessionID = "session-A"

	m, _ := newTestModel(t)
	m.supervisor = supervisor.New(nil)
	_, err := m.supervisor.AddSession(t.Context(), nil, &session.Session{ID: sessionID}, "/tmp", nil)
	require.NoError(t, err)
	m.sessionStates[sessionID] = service.NewSessionState(&session.Session{ID: sessionID})

	first := &runtime.ElicitationRequestEvent{Message: "worker1 needs input", ElicitationID: "e1"}
	second := &runtime.ElicitationRequestEvent{Message: "worker2 needs input", ElicitationID: "e2"}
	runner := m.supervisor.GetRunner(sessionID)
	require.NotNil(t, runner)
	runner.PendingEvents = []tea.Msg{first, second}

	cmd := m.replayPendingEvent(sessionID)
	require.NotNil(t, cmd)

	msgs := drainInOrder(t, cmd)
	require.Len(t, msgs, 2, "both queued elicitations must be replayed")

	open1, ok := msgs[0].(dialog.OpenDialogMsg)
	require.True(t, ok, "expected dialog.OpenDialogMsg, got %T", msgs[0])
	open2, ok := msgs[1].(dialog.OpenDialogMsg)
	require.True(t, ok, "expected dialog.OpenDialogMsg, got %T", msgs[1])

	assert.Same(t, first, open1.OriginatingEvent, "the first-queued elicitation must be replayed first")
	assert.Same(t, second, open2.OriginatingEvent, "the second-queued elicitation must be replayed second, not first")
}
