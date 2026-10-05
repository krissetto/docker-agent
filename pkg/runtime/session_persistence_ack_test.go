package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestCanonicalSettlementWaitsForPersistenceAcknowledgement(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.sessionDrivers.closed = true
	d := newSessionDriver(r, session.New(session.WithID("owner")))
	d.generation, d.activeRequestID, d.phase = 1, "accepted", sessionRunning
	observer := newPersistenceObserver(session.NewInMemorySessionStore())
	r.observers = []EventObserver{observer}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	observer.journal("owner").pending = []persistenceEffect{{write: func(context.Context) error { close(entered); <-release; return nil }}}
	d.events.SetRequest("owner", "accepted", 1)
	d.events.Publish("owner", StreamStopped("owner", "root", "normal"))
	go func() { d.finishRunContext(t.Context(), 1, ""); close(done) }()
	<-entered
	require.Empty(t, canonicalSettlements(canonicalReplay(d)), "provider stop cannot publish settlement before persistence acknowledgement")
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		require.Equal(t, sessionSettling, d.phase)
		require.Equal(t, "accepted", d.activeRequestID)
		return nil
	}))
	close(release)
	<-done
	require.Len(t, canonicalSettlements(canonicalReplay(d)), 1)
	require.NoError(t, d.ownerCall(t.Context(), func() error { require.Equal(t, sessionIdle, d.phase); require.Empty(t, d.activeRequestID); return nil }))
}
