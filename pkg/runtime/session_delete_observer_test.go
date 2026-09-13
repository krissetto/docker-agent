package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func observeUntilClosed(t *testing.T, obs Observation) []*StreamStoppedEvent {
	t.Helper()
	var stopped []*StreamStoppedEvent
	for {
		select {
		case envelope, ok := <-obs.Events:
			if !ok {
				return stopped
			}
			if event, ok := envelope.Event.(*StreamStoppedEvent); ok {
				stopped = append(stopped, event)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("observer did not close")
		}
	}
}

func TestDeleteSessionFreshSettledPublishesDeletedAndClosesObservers(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)

	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))
	stopped := observeUntilClosed(t, obs)
	require.Len(t, stopped, 1)
	assert.Equal(t, "deleted", stopped[0].Reason)

	_, err = h.Observe(t.Context(), ObserveOptions{})
	require.ErrorIs(t, err, ErrSessionStopped)
}

func TestDeleteSessionAfterSettledRunDoesNotDuplicateTerminal(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	_, err = h.Submit(t.Context(), TurnInput{Content: "settle"})
	require.NoError(t, err)

	var stopped []*StreamStoppedEvent
	for len(stopped) == 0 {
		select {
		case envelope := <-obs.Events:
			if event, ok := envelope.Event.(*StreamStoppedEvent); ok {
				stopped = append(stopped, event)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("run did not settle")
		}
	}
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))
	stopped = append(stopped, observeUntilClosed(t, obs)...)
	assert.Len(t, stopped, 1)
}

func TestDeleteSessionClosesMultipleObserversExactlyOnce(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	observers := make([]Observation, 4)
	for i := range observers {
		observers[i], err = h.Observe(t.Context(), ObserveOptions{})
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			assert.NoError(t, rt.DeleteSession(t.Context(), sess.ID))
		})
	}
	wg.Wait()
	for _, obs := range observers {
		stopped := observeUntilClosed(t, obs)
		require.Len(t, stopped, 1)
		assert.Equal(t, "deleted", stopped[0].Reason)
	}
}

func TestConcurrentObserveAndDeleteNeverLeavesObserverOpen(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)

	start := make(chan struct{})
	results := make(chan Observation, 32)
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			<-start
			obs, observeErr := h.Observe(t.Context(), ObserveOptions{})
			if observeErr != nil {
				errs <- observeErr
				return
			}
			results <- obs
		})
	}
	close(start)
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))
	wg.Wait()
	close(results)
	close(errs)
	for observeErr := range errs {
		require.ErrorIs(t, observeErr, ErrSessionStopped)
	}
	for obs := range results {
		stopped := observeUntilClosed(t, obs)
		require.Len(t, stopped, 1)
		assert.Equal(t, "deleted", stopped[0].Reason)
	}
}

func TestDeleteSessionTimeoutCanResumeFinalization(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	d, ok := rt.sessionDrivers.Lookup(sess.ID)
	require.True(t, ok)
	obs, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)

	d.wg.Add(1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, rt.DeleteSession(ctx, sess.ID), context.Canceled)
	select {
	case <-obs.Events:
		t.Fatal("timed out delete closed observer while publisher remained")
	default:
	}
	d.wg.Done()
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))
	stopped := observeUntilClosed(t, obs)
	require.Len(t, stopped, 1)
	assert.Equal(t, "deleted", stopped[0].Reason)
}

func TestReleaseSessionKeepsObserversOpenAndSessionAttachable(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)

	require.NoError(t, rt.ReleaseSession(t.Context(), sess.ID))
	select {
	case event, ok := <-obs.Events:
		if !ok {
			t.Fatal("release closed observer")
		}
		if stopped, ok := event.Event.(*StreamStoppedEvent); ok {
			t.Fatalf("release emitted terminal %q", stopped.Reason)
		}
	case <-time.After(50 * time.Millisecond):
	}
	_, err = rt.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	require.NoError(t, err)
	obs.Cancel()
}
