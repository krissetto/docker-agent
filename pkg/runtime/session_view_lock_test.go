package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

// A prepared view keeps its root's transition locks until it commits or
// aborts. A second prepare for the same root waits only as long as its context
// allows, and proceeds once the first view releases.
func TestPreparedViewHoldsTransitionUntilAbort(t *testing.T) {
	service := NewSessionService()
	store := session.NewInMemorySessionStore()
	r := serviceRuntime(t, service, store)
	id := t.Name()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID(id), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))))
	view := &localSessionRuntimeView{runtime: r}

	first, err := view.PrepareSessionView(t.Context(), id)
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = view.PrepareSessionView(waitCtx, id)
	require.ErrorIs(t, err, context.DeadlineExceeded, "a waiter must give up when its own context ends")

	first.Abort()
	second, err := view.PrepareSessionView(t.Context(), id)
	require.NoError(t, err, "abort must release the transition locks")
	second.Abort()
}

// Committing publishes the view and releases the locks, so the same root can be
// prepared again in the same goroutine without self-deadlock.
func TestPreparedViewCommitReleasesTransitionForSameRoot(t *testing.T) {
	service := NewSessionService()
	store := session.NewInMemorySessionStore()
	r := serviceRuntime(t, service, store)
	id := t.Name()
	require.NoError(t, store.AddSession(t.Context(), session.New(session.WithID(id), session.WithAgentName("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))))
	view := &localSessionRuntimeView{runtime: r}

	first, err := view.PrepareSessionView(t.Context(), id)
	require.NoError(t, err)
	committed, err := first.Commit(t.Context())
	require.NoError(t, err)
	require.Equal(t, id, committed.Info.SessionID)
	first.Abort()

	again, err := view.PrepareSessionView(t.Context(), id)
	require.NoError(t, err)
	again.Abort()
}
