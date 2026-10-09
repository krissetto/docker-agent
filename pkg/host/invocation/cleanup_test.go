package invocation

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupBackoffRetainsLatestCauseWithoutOverlap(t *testing.T) {
	t.Parallel()
	g := New(nil)
	ctx, done, err := g.Begin(t.Context())
	require.NoError(t, err)
	defer done()
	firstErr := errors.New("first shutdown failed")
	lastErr := errors.New("last shutdown failed")
	delays := make(chan time.Duration)
	release := make(chan struct{})
	finished := make(chan struct{})
	var attempts, concurrent, maximum atomic.Int32
	go func() {
		defer close(finished)
		g.joinCleanup(ctx, func(context.Context) error {
			active := concurrent.Add(1)
			maximum.Store(max(maximum.Load(), active))
			defer concurrent.Add(-1)
			n := attempts.Add(1)
			if n == 1 {
				return firstErr
			}
			if n <= 8 {
				return lastErr
			}
			return nil
		}, func(delay time.Duration) {
			delays <- delay
			<-release
		})
	}()
	for i, expected := range []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 5 * time.Second, 5 * time.Second} {
		require.Equal(t, expected, <-delays)
		require.EqualValues(t, i+1, attempts.Load(), "no retry while delay remains blocked")
		expired, cancel := context.WithCancel(t.Context())
		cancel()
		err := g.Close(expired)
		require.ErrorIs(t, err, context.Canceled)
		if i == 0 {
			require.ErrorIs(t, err, firstErr)
		} else {
			require.ErrorIs(t, err, lastErr)
			require.NotErrorIs(t, err, firstErr)
		}
		release <- struct{}{}
	}
	<-finished
	require.EqualValues(t, 1, maximum.Load())
	g.mu.Lock()
	defer g.mu.Unlock()
	assert.Empty(t, g.cleanupErrors)
}

func TestFailedSupervisorCleanupRetainsInvocation(t *testing.T) {
	t.Parallel()
	var closes atomic.Int32
	g := New(func() error { closes.Add(1); return nil })
	ctx, done, err := g.Begin(t.Context())
	require.NoError(t, err)
	failed := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	go func() {
		defer done()
		g.JoinCleanup(ctx, func(context.Context) error {
			if attempts.Add(1) == 1 {
				close(failed)
				return errors.New("supervisor cleanup failed")
			}
			<-release
			return nil
		})
	}()
	<-failed
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	var drainErr *DrainError
	require.ErrorAs(t, g.Close(expired), &drainErr)
	assert.Zero(t, closes.Load())
	close(release)
	require.NoError(t, drainErr.Retry(t.Context()))
	assert.EqualValues(t, 2, attempts.Load())
	assert.EqualValues(t, 1, closes.Load())
}
