package supervisor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func newTestSupervisor(ids []string, activeID string) *Supervisor {
	s := &Supervisor{
		runners:       make(map[string]*SessionTab),
		programReady:  make(chan struct{}),
		tabNotify:     make(chan struct{}, 1),
		tabNotifyDone: make(chan struct{}),
	}
	for _, id := range ids {
		s.runners[id] = &SessionTab{ID: id}
		s.order = append(s.order, id)
	}
	s.activeID = activeID
	return s
}

func TestAddSessionRejectsDuplicateIDWithoutChangingOrder(t *testing.T) {
	t.Parallel()
	s := New(nil)
	first := session.New(session.WithID("duplicate"))

	id, err := s.AddSession(t.Context(), nil, first, "first", nil)
	require.NoError(t, err)
	assert.Equal(t, first.ID, id)

	var duplicateCleanup atomic.Int32
	id, err = s.AddSession(t.Context(), nil, session.New(session.WithID(first.ID)), "second", func() { duplicateCleanup.Add(1) })
	require.EqualError(t, err, `session "duplicate" is already supervised`)
	assert.Empty(t, id)
	assert.Equal(t, int32(1), duplicateCleanup.Load())
	assert.Equal(t, []string{first.ID}, s.order)
	assert.Equal(t, "first", s.runners[first.ID].WorkingDir)
}

func TestRetargetRouteStopsOnShutdown(t *testing.T) {
	t.Parallel()
	s := New(nil)
	sess := session.New(session.WithID("old"))
	a := app.New(t.Context(), nil, sess, runtime.SessionBinding{})
	s.runners[sess.ID] = &SessionTab{ID: sess.ID, App: a, lifetimeCtx: context.Background()}
	s.order = []string{sess.ID}
	s.activeID = sess.ID
	require.True(t, s.RetargetRunner(t.Context(), sess.ID, "new", "new-dir"))

	s.mu.RLock()
	routeDone := s.runners["new"].routeDone
	s.mu.RUnlock()
	require.NotNil(t, routeDone)

	s.Shutdown()
	select {
	case <-routeDone:
	case <-time.After(time.Second):
		t.Fatal("retargeted routing goroutine did not exit after shutdown")
	}
}

func TestCloseSession_FocusesPreviousTab(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"A", "B", "C"}, "C")

	next := s.CloseSession("C")

	assert.Equal(t, "B", next)
	assert.Equal(t, "B", s.activeID)
	assert.Equal(t, []string{"A", "B"}, s.order)
}

func TestCloseSession_FocusesPreviousTab_Middle(t *testing.T) {
	t.Parallel()
	// Tabs: [A, B, C], active=B. Close B → expect A.
	s := newTestSupervisor([]string{"A", "B", "C"}, "B")

	next := s.CloseSession("B")

	assert.Equal(t, "A", next)
	assert.Equal(t, "A", s.activeID)
	assert.Equal(t, []string{"A", "C"}, s.order)
}

func TestCloseSession_FirstTab_FocusesNewFirst(t *testing.T) {
	t.Parallel()
	// Tabs: [A, B, C], active=A. Close A → expect B (new first).
	s := newTestSupervisor([]string{"A", "B", "C"}, "A")

	next := s.CloseSession("A")

	assert.Equal(t, "B", next)
	assert.Equal(t, "B", s.activeID)
	assert.Equal(t, []string{"B", "C"}, s.order)
}

func TestCloseSession_LastRemaining(t *testing.T) {
	t.Parallel()
	// Tabs: [A], active=A. Close A → expect "".
	s := newTestSupervisor([]string{"A"}, "A")

	next := s.CloseSession("A")

	assert.Empty(t, next)
	assert.Empty(t, s.activeID)
	assert.Empty(t, s.order)
}

func TestCloseSession_InactiveTab(t *testing.T) {
	t.Parallel()
	// Tabs: [A, B, C], active=A. Close C → active stays A.
	s := newTestSupervisor([]string{"A", "B", "C"}, "A")

	next := s.CloseSession("C")

	assert.Equal(t, "A", next)
	assert.Equal(t, "A", s.activeID)
	assert.Equal(t, []string{"A", "B"}, s.order)
}

func TestCloseSession_NonExistent(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"A", "B"}, "A")

	next := s.CloseSession("Z")

	assert.Equal(t, "A", next)
	assert.Equal(t, []string{"A", "B"}, s.order)
}

func TestCloseSession_TwoTabs_CloseSecond(t *testing.T) {
	t.Parallel()
	// Tabs: [A, B], active=B. Close B → expect A.
	s := newTestSupervisor([]string{"A", "B"}, "B")

	next := s.CloseSession("B")

	assert.Equal(t, "A", next)
	assert.Equal(t, "A", s.activeID)
	assert.Equal(t, []string{"A"}, s.order)
}

// TestSetPendingEvent_RoundTrip verifies that SetPendingEvent stores an event
// for a session and that ConsumePendingEvent retrieves and clears it. This
// is the path used to re-stash a background dialog's originating event when
// the user switches away from the tab that opened it (see #2626).
func TestSetPendingEvent_RoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"A", "B"}, "A")

	type fakeEvent struct{ id int }
	event := &fakeEvent{id: 7}

	s.SetPendingEvent("A", event)

	assert.Equal(t, []tea.Msg{event}, s.runners["A"].PendingEvents, "event is stored on the runner")
	assert.False(t, s.runners["A"].NeedsAttn, "SetPendingEvent must NOT raise NeedsAttn (the user is already aware)")

	got := s.ConsumePendingEvent("A")
	assert.Equal(t, event, got)
	assert.Empty(t, s.runners["A"].PendingEvents, "event is cleared after consumption")
}

// TestSetPendingEvent_Queue verifies that multiple events queued for the same
// inactive session are replayed in FIFO order and that SetPendingEvent
// re-queues at the front, ahead of anything queued behind it (#3584).
func TestSetPendingEvent_Queue(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"A"}, "B")

	type fakeEvent struct{ id int }
	first := &fakeEvent{id: 1}
	second := &fakeEvent{id: 2}

	s.runners["A"].PendingEvents = []tea.Msg{first, second}

	// Re-stash a third event (e.g. the live dialog instance for the event the
	// user was looking at) ahead of the two already queued.
	stashed := &fakeEvent{id: 0}
	s.SetPendingEvent("A", stashed)

	assert.Equal(t, stashed, s.ConsumePendingEvent("A"))
	assert.Equal(t, first, s.ConsumePendingEvent("A"))
	assert.Equal(t, second, s.ConsumePendingEvent("A"))
	assert.Nil(t, s.ConsumePendingEvent("A"), "queue is drained")
}

// TestSetPendingEvent_UnknownSession is a no-op (and must not panic).
func TestSetPendingEvent_UnknownSession(t *testing.T) {
	t.Parallel()
	s := newTestSupervisor([]string{"A"}, "A")

	s.SetPendingEvent("does-not-exist", "payload")

	assert.Empty(t, s.runners["A"].PendingEvents, "unrelated runner is untouched")
}
