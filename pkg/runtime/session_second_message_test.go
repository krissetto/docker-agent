package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestSecondSubmissionDuringSettlementDoesNotReportMailboxCapacity(t *testing.T) {
	store := coordinationSQLite(t)
	rt, owner := coordinationRuntime(t, store, coordinationReply("answer"), coordinationReply("worker"))
	sess := session.New(session.WithID("second-message"), session.WithTitle("history"))
	for range 100 {
		sess.AddMessage(session.UserMessage("old history"))
	}
	h, err := owner.Runtime().CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	first, err := h.Submit(t.Context(), TurnInput{Content: "first"})
	require.NoError(t, err)
	coordinationAwait(t, h, first.TurnID)
	d := h.(*sessionHandle).driver
	// A finished provider is not yet a committed session turn. Exercise the
	// public second-submit boundary while that durability barrier is held.
	d.mu.Lock()
	d.settling = true
	d.mu.Unlock()
	second, err := h.Submit(t.Context(), TurnInput{Content: "second"})
	require.NoError(t, err)
	assert.Equal(t, SubmissionDispositionQueued, second.Disposition)
	assert.NotContains(t, d.LastError(), "session start:")
	d.mu.Lock()
	d.settling = false
	d.mu.Unlock()
	rt.sessionDrivers.signalWork()
	coordinationAwait(t, h, second.TurnID)
	obs, err := h.Observe(t.Context(), ObserveOptions{Since: new(uint64)})
	require.NoError(t, err)
	defer obs.Cancel()
	for _, event := range obs.Replay {
		if failure, ok := event.Event.(*ErrorEvent); ok {
			assert.NotContains(t, failure.Error, "session start:")
		}
	}
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	count := 0
	for _, item := range snapshot.Messages {
		if item.Message != nil && item.Message.TurnID == second.TurnID {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestCompetingWakeDoesNotPublishFalseStartCapacity(t *testing.T) {
	rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("answer"), coordinationReply("worker"))
	h := coordinationCreate(t, owner.Runtime(), "wake-race", "")
	d := h.(*sessionHandle).driver
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	d.beforePrepareStart = func() { once.Do(func() { close(entered); <-release }) }
	done := make(chan error, 1)
	go func() { _, err := h.Submit(t.Context(), TurnInput{Content: "first"}); done <- err }()
	<-entered
	// Another scheduler wins the wake and runs while the submitter is between
	// durable append and start. Its request remains accepted, never a limit error.
	d.mu.Lock()
	d.starting = true
	d.startDone = make(chan struct{})
	d.mu.Unlock()
	close(release)
	require.NoError(t, <-done)
	assert.NotContains(t, d.LastError(), "session start:")
	d.mu.Lock()
	d.starting = false
	d.signalStartDoneLocked()
	d.mu.Unlock()
	d.WakePending()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	obs, err := h.Observe(ctx, ObserveOptions{Since: new(uint64)})
	require.NoError(t, err)
	defer obs.Cancel()
	for _, event := range obs.Replay {
		if e, ok := event.Event.(*ErrorEvent); ok {
			assert.NotContains(t, e.Error, "session start:")
		}
	}
	_ = rt
}
