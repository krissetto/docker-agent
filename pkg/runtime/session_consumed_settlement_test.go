package runtime

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestConsumedOnlyTerminalEvidenceSurvivesRestart(t *testing.T) {
	for _, kind := range []string{"completed", "failed", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "consumed-only.db")
			base, err := sqlitestore.New(t.Context(), path)
			require.NoError(t, err)
			defer func() { require.NoError(t, base.Close()) }()
			wrapped := &recoveryMetadataStore{Store: base}
			r := newPersistedSessionRuntime(t, wrapped)
			h, err := r.CreateSession(t.Context(), session.New(session.WithID("consumed-only")), SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			r.sessionDrivers.mu.Lock()
			r.sessionDrivers.closed = true
			r.sessionDrivers.mu.Unlock()
			d := h.(*sessionHandle).driver
			_, err = d.admitInput(t.Context(), QueuedMessage{RequestID: "consumed", Content: "guide", InputMode: "steer", InputOrigin: session.InputOriginUser}, SessionOperationSteer, true, true)
			require.NoError(t, err)
			require.Len(t, d.DrainSteering(), 1)
			require.NoError(t, d.ownerCall(t.Context(), func() error {
				d.generation = 1
				d.phase = sessionRunning
				if kind == "canceled" {
					d.phase = sessionCancelling
				}
				return nil
			}))
			wrapped.failNext = true
			runErr := ""
			expected := TurnCompleted
			if kind == "failed" {
				runErr = "provider failed"
				expected = TurnFailed
			}
			if kind == "canceled" {
				expected = TurnCanceled
			}
			d.finishRun(1, runErr)
			require.Error(t, d.completionErr, "consumed-only settlement must attempt persistence")
			require.Len(t, d.consumedSteering, 1, "failed persistence must not lose identity")
			failed, err := base.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			require.Empty(t, failed.TurnOutcomesSnapshot())
			require.ErrorIs(t, h.AwaitTurn(t.Context(), "consumed"), d.completionErr)
			require.Empty(t, canonicalSettlements(canonicalReplay(d)))
			d.finishRun(1, runErr)
			require.NoError(t, d.completionErr)
			require.Empty(t, d.consumedSteering)
			require.NoError(t, r.shutdownSessions(context.WithoutCancel(t.Context())))
			require.NoError(t, base.Close())
			base, err = sqlitestore.New(t.Context(), path)
			require.NoError(t, err)
			loaded, err := base.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			require.Equal(t, map[string]string{"consumed": string(expected)}, loaded.TurnOutcomesSnapshot(), "no empty-key phantom primary")
			restored := newSessionDriver(newDriverTestRuntime(t), loaded)
			defer restored.closeOwner()
			require.Zero(t, restored.Status().InterruptedTurns)
			require.Empty(t, canonicalSettlements(canonicalReplay(d)), "no invented primary settlement event")
		})
	}
}

func TestConsumedOnlySettlementConcurrentSteeringRetry(t *testing.T) {
	base := session.NewInMemorySessionStore()
	r := newPersistedSessionRuntime(t, base)
	h, err := r.CreateSession(t.Context(), session.New(session.WithID("settlement-boundary")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	r.sessionDrivers.mu.Lock()
	r.sessionDrivers.closed = true
	r.sessionDrivers.mu.Unlock()
	d := h.(*sessionHandle).driver
	first, err := h.Steer(t.Context(), TurnInput{Content: "first guidance", RequestID: "first"})
	require.NoError(t, err)
	require.Len(t, d.DrainSteering(), 1)
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.generation = 1; d.phase = sessionRunning; return nil }))

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	observer := newPersistenceObserver(base)
	observer.journal(h.ID()).pending = []persistenceEffect{{write: func(context.Context) error {
		close(entered)
		<-release
		return nil
	}}}
	r.observers = []EventObserver{observer}
	type successor struct {
		generation uint64
		again      bool
	}
	finished := make(chan successor, 1)
	go func() {
		_, generation, again := d.finishRun(1, "")
		finished <- successor{generation, again}
	}()
	coordinationWait(t, entered)
	lateInput := TurnInput{Content: "late guidance", RequestID: "late"}
	late, err := h.Steer(t.Context(), lateInput)
	require.NoError(t, err)
	require.Empty(t, late.Disposition)
	type result struct {
		submission Submission
		err        error
	}
	retries := make(chan result, 8)
	for range 8 {
		go func() { submission, err := h.Steer(t.Context(), lateInput); retries <- result{submission, err} }()
	}
	for range 8 {
		retry := <-retries
		require.NoError(t, retry.err)
		require.Equal(t, late, retry.submission, "exact retries retain the STEERING receipt")
	}
	var pending, steering int
	var consumed []string
	readLanes := func() error {
		pending, steering = len(d.pending), len(d.steering)
		consumed = slices.Clone(d.consumedSteering)
		return nil
	}
	require.NoError(t, d.ownerCall(t.Context(), readLanes))
	require.Zero(t, pending)
	require.Equal(t, 1, steering)
	require.Equal(t, []string{first.TurnID}, consumed)
	once.Do(func() { close(release) })
	next := <-finished
	require.True(t, next.again, "accepted boundary STEERING must have a successor")
	require.Equal(t, uint64(2), next.generation)
	require.Equal(t, late.TurnID, d.ActiveRequestID())
	loaded, err := base.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	require.Equal(t, map[string]string{first.TurnID: string(TurnCompleted)}, loaded.TurnOutcomesSnapshot(), "late input is not falsely settled with prior consumed inputs")
	require.NoError(t, d.ownerCall(t.Context(), readLanes))
	require.Zero(t, pending)
	require.Zero(t, steering)
	require.Equal(t, []string{late.TurnID}, consumed)
	d.finishRun(next.generation, "")
	d.finishRun(next.generation, "")
	loaded, err = base.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	require.Equal(t, map[string]string{first.TurnID: string(TurnCompleted), late.TurnID: string(TurnCompleted)}, loaded.TurnOutcomesSnapshot())
	counts := map[string]int{}
	for _, item := range loaded.MessagesSnapshot() {
		if item.Message != nil && item.Message.Accepted {
			counts[item.Message.TurnID]++
			require.Equal(t, "steer", item.Message.InputMode, "boundary continuation preserves STEERING semantics")
			require.False(t, item.Message.Pending)
		}
	}
	require.Equal(t, map[string]int{first.TurnID: 1, late.TurnID: 1}, counts)
	require.NoError(t, h.AwaitTurn(t.Context(), late.TurnID))
	require.Len(t, canonicalSettlements(canonicalReplay(d)), 1, "only the successor with an actual primary identity emits settlement")
	restored := newSessionDriver(newDriverTestRuntime(t), loaded)
	defer restored.closeOwner()
	require.Zero(t, restored.Status().InterruptedTurns)
	require.Empty(t, restored.pending)
	require.Empty(t, restored.steering)
}
