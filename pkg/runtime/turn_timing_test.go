package runtime

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/tools"
)

// turnTimingHarness drives one root turn through the real owner paths with a
// fake clock, so every duration is exact.
type turnTimingHarness struct {
	t     *testing.T
	r     *LocalRuntime
	d     *sessionDriver
	clock *fakeClock
	sess  *session.Session
}

func newTurnTimingHarness(t *testing.T, store session.Store, origin session.InputOrigin) *turnTimingHarness {
	t.Helper()
	r := newDriverTestRuntime(t)
	r.sessionDrivers.closed = true
	clock := newFakeClock(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC))
	r.now = clock.Now
	if store != nil {
		r.sessionStore = store
	}
	sess := session.New(session.WithID("owner"))
	if store != nil {
		require.NoError(t, store.AddSession(t.Context(), sess))
	}
	d := newSessionDriver(r, sess)
	d.mu.Lock()
	d.generation, d.activeRequestID, d.phase = 1, "turn", sessionRunning
	d.startTurnClockLocked("turn", origin)
	d.mu.Unlock()
	return &turnTimingHarness{t: t, r: r, d: d, clock: clock, sess: sess}
}

// openApproval registers a tool approval the way the tool dispatcher does.
func (h *turnTimingHarness) openApproval(id string) {
	h.t.Helper()
	_, err := h.d.registerResume(h.t.Context(), id)
	require.NoError(h.t, err)
}

// respondApproval resolves an approval through the public Respond path.
func (h *turnTimingHarness) respondApproval(id string) {
	h.t.Helper()
	require.NoError(h.t, h.d.Respond(InteractionResponse{InteractionID: id, Kind: InteractionConfirmation, Resume: ResumeApprove()}))
}

// openQuestion registers an elicitation the way the elicitation tool does.
func (h *turnTimingHarness) openQuestion(id string) {
	h.t.Helper()
	require.NoError(h.t, h.d.registerElicitation(h.t.Context(), id, &ElicitationRequestEvent{ElicitationID: id, Message: "why?"}, newElicitationWaiter()))
}

// answerQuestion resolves an elicitation through the public Respond path.
func (h *turnTimingHarness) answerQuestion(id string) {
	h.t.Helper()
	require.NoError(h.t, h.d.Respond(InteractionResponse{InteractionID: id, Kind: InteractionElicitation, ElicitationID: id, Elicitation: ElicitationResult{Action: tools.ElicitationActionAccept}}))
}

// settle runs the turn's settlement and returns the published TurnSettledEvent.
func (h *turnTimingHarness) settle(runErr string) *TurnSettledEvent {
	h.t.Helper()
	h.d.finishRunContext(h.t.Context(), 1, runErr)
	events := canonicalSettlements(canonicalReplay(h.d))
	require.Len(h.t, events, 1)
	return events[0].Event.(*TurnSettledEvent)
}

func expectedTiming(outcome TurnOutcome, durationMs, waitingMs int64) session.TurnTiming {
	return session.TurnTiming{Version: session.TurnTimingVersion, Basis: session.TurnTimingBasisActiveWall, TurnID: "turn", DurationMs: durationMs, WaitingMs: waitingMs, Outcome: string(outcome)}
}

func TestTurnTimingSubtractsApprovalWait(t *testing.T) {
	t.Parallel()
	h := newTurnTimingHarness(t, nil, session.InputOriginUser)
	h.clock.Advance(time.Second)
	h.openApproval("approval")
	h.clock.Advance(2 * time.Second)
	h.respondApproval("approval")
	h.clock.Advance(time.Second)

	event := h.settle("")
	require.NotNil(t, event.Timing)
	assert.Equal(t, expectedTiming(TurnCompleted, 2000, 2000), *event.Timing)
	timing, ok := h.sess.TurnTiming("turn")
	require.True(t, ok)
	assert.Equal(t, *event.Timing, timing)
}

func TestTurnTimingSubtractsQuestionWait(t *testing.T) {
	t.Parallel()
	h := newTurnTimingHarness(t, nil, session.InputOriginUser)
	h.clock.Advance(500 * time.Millisecond)
	h.openQuestion("question")
	h.clock.Advance(1500 * time.Millisecond)
	h.answerQuestion("question")
	h.clock.Advance(250 * time.Millisecond)

	event := h.settle("")
	require.NotNil(t, event.Timing)
	assert.Equal(t, expectedTiming(TurnCompleted, 750, 1500), *event.Timing)
}

// Overlapping waits are wall time once: approval [1s,3s] and question [2s,4s]
// block for 3s, not the 4s a per-interaction sum would report.
func TestTurnTimingCountsOverlappingWaitsOnce(t *testing.T) {
	t.Parallel()
	h := newTurnTimingHarness(t, nil, session.InputOriginUser)
	h.clock.Advance(time.Second)
	h.openApproval("approval")
	h.clock.Advance(time.Second)
	h.openQuestion("question")
	h.clock.Advance(time.Second)
	h.respondApproval("approval")
	h.clock.Advance(time.Second)
	h.answerQuestion("question")
	h.clock.Advance(time.Second)

	event := h.settle("")
	require.NotNil(t, event.Timing)
	assert.Equal(t, expectedTiming(TurnCompleted, 2000, 3000), *event.Timing)
}

// Without waits the duration is the elapsed span of the turn; concurrent work
// inside the span is never summed into it.
func TestTurnTimingIsElapsedWallTimeWithoutWaits(t *testing.T) {
	t.Parallel()
	h := newTurnTimingHarness(t, nil, session.InputOriginUser)
	h.clock.Advance(3 * time.Second)

	event := h.settle("")
	require.NotNil(t, event.Timing)
	assert.Equal(t, expectedTiming(TurnCompleted, 3000, 0), *event.Timing)
}

// Stopping while blocked on approval ends the wait at settlement and reports canceled.
func TestTurnTimingStoppedWhileWaiting(t *testing.T) {
	t.Parallel()
	h := newTurnTimingHarness(t, nil, session.InputOriginUser)
	h.clock.Advance(time.Second)
	h.openApproval("approval")
	h.clock.Advance(2 * time.Second)
	h.d.mu.Lock()
	h.d.stopped = true
	h.d.mu.Unlock()
	h.clock.Advance(time.Second)

	event := h.settle("")
	assert.Equal(t, TurnCanceled, event.Outcome)
	require.NotNil(t, event.Timing)
	assert.Equal(t, expectedTiming(TurnCanceled, 1000, 3000), *event.Timing)
}

func TestTurnTimingFailedOutcome(t *testing.T) {
	t.Parallel()
	h := newTurnTimingHarness(t, nil, session.InputOriginUser)
	h.clock.Advance(1500 * time.Millisecond)

	event := h.settle("model failed")
	assert.Equal(t, TurnFailed, event.Outcome)
	require.NotNil(t, event.Timing)
	assert.Equal(t, expectedTiming(TurnFailed, 1500, 0), *event.Timing)
}

// Settlement retries reuse the first frozen end, so a failed durable write does not inflate the duration.
func TestTurnTimingFrozenAcrossSettlementRetry(t *testing.T) {
	t.Parallel()
	h := newTurnTimingHarness(t, nil, session.InputOriginUser)
	h.clock.Advance(time.Second)
	h.d.mu.Lock()
	first, ok := h.d.finishTurnClockLocked("turn", TurnCompleted)
	h.d.mu.Unlock()
	require.True(t, ok)
	h.clock.Advance(5 * time.Second)
	h.d.mu.Lock()
	second, ok := h.d.finishTurnClockLocked("turn", TurnCompleted)
	h.d.mu.Unlock()
	require.True(t, ok)
	assert.Equal(t, first, second)
	assert.Equal(t, int64(1000), second.DurationMs)
}

// Non-human turns are never measured, so they get no timing.
func TestTurnTimingSkipsNonUserTurns(t *testing.T) {
	t.Parallel()
	h := newTurnTimingHarness(t, nil, session.InputOriginAgent)
	h.clock.Advance(time.Second)

	event := h.settle("")
	assert.Nil(t, event.Timing)
	_, ok := h.sess.TurnTiming("turn")
	assert.False(t, ok)
}

func TestTurnTimingPersistsAndReloads(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	h := newTurnTimingHarness(t, store, session.InputOriginUser)
	h.clock.Advance(time.Second)
	h.openApproval("approval")
	h.clock.Advance(2 * time.Second)
	h.respondApproval("approval")
	h.clock.Advance(time.Second)
	event := h.settle("")
	require.NotNil(t, event.Timing)
	require.NoError(t, store.Close())

	reopened, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	loaded, err := reopened.GetSession(t.Context(), "owner")
	require.NoError(t, err)
	timing, ok := loaded.TurnTiming("turn")
	require.True(t, ok)
	assert.Equal(t, *event.Timing, timing)
	assert.Equal(t, expectedTiming(TurnCompleted, 2000, 2000), timing)
}

// A turn settled before timing existed keeps its outcome but has no timing after reload.
func TestTurnTimingLegacyRowHasNoTiming(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	legacy := session.New(session.WithID("legacy"))
	legacy.SetTurnOutcome("turn", string(TurnCompleted))
	require.NoError(t, store.AddSession(t.Context(), legacy))
	reloaded, err := store.GetSession(t.Context(), "legacy")
	require.NoError(t, err)
	assert.Equal(t, string(TurnCompleted), reloaded.TurnOutcome("turn"))
	_, ok := reloaded.TurnTiming("turn")
	assert.False(t, ok)
	require.NoError(t, store.Close())
}

func TestTurnSettledEventTimingJSONRoundTrip(t *testing.T) {
	t.Parallel()
	timing := expectedTiming(TurnCanceled, 1234, 567)
	event := &TurnSettledEvent{Type: "turn_settled", SessionID: "owner", TurnID: "turn", Outcome: TurnCanceled, Timing: &timing}
	data, err := json.Marshal(event)
	require.NoError(t, err)

	client, err := NewClient("http://localhost:1")
	require.NoError(t, err)
	decoded := client.registry["turn_settled"]()
	require.NoError(t, json.Unmarshal(data, decoded))
	assert.Equal(t, event, decoded)

	untimed, err := json.Marshal(&TurnSettledEvent{Type: "turn_settled", SessionID: "owner", TurnID: "turn", Outcome: TurnCompleted})
	require.NoError(t, err)
	assert.NotContains(t, string(untimed), "timing")
}
