package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionDriverPauseStateCycles(t *testing.T) {
	d := newSessionDriver(&LocalRuntime{}, nil)
	got, err := d.TogglePause(t.Context())
	require.NoError(t, err)
	assert.True(t, got)
	got, err = d.TogglePause(t.Context())
	require.NoError(t, err)
	assert.False(t, got)
}

func TestSessionDriverPauseIndependentSessions(t *testing.T) {
	a := newSessionDriver(&LocalRuntime{}, nil)
	b := newSessionDriver(&LocalRuntime{}, nil)
	_, err := a.TogglePause(t.Context())
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := a.waitIfPaused(t.Context(), nil)
		done <- err
	}()

	blocked, err := b.waitIfPaused(t.Context(), nil)
	require.NoError(t, err)
	assert.False(t, blocked, "a paused session must not pause another session")
	select {
	case <-done:
		t.Fatal("paused session resumed early")
	case <-time.After(10 * time.Millisecond):
	}
	_, err = a.TogglePause(t.Context())
	require.NoError(t, err)
	require.NoError(t, <-done)
}

func TestSessionDriverPauseConcurrentToggleWaiters(t *testing.T) {
	d := newSessionDriver(&LocalRuntime{}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 100 {
				_, _ = d.TogglePause(ctx)
			}
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				_, _ = d.waitIfPaused(ctx, nil)
			}
		}()
	}
	time.AfterFunc(20*time.Millisecond, cancel)
	wg.Wait()
}

func TestSessionDriverPauseCancellation(t *testing.T) {
	d := newSessionDriver(&LocalRuntime{}, nil)
	_, err := d.TogglePause(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reached := make(chan struct{})
	type waitResult struct {
		blocked bool
		err     error
	}
	done := make(chan waitResult, 1)
	// The pause was published while idle; reopen its boundary notification.
	d.mu.Lock()
	d.pausePublished = 0
	d.mu.Unlock()
	go func() {
		blocked, err := d.waitIfPaused(ctx, func() { close(reached) })
		done <- waitResult{blocked: blocked, err: err}
	}()
	<-reached
	cancel()
	result := <-done
	assert.True(t, result.blocked)
	require.ErrorIs(t, result.err, context.Canceled)
}
