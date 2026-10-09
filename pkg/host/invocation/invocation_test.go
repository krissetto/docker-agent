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

func TestDrainRetainsDependenciesAndRetriesExactlyOnce(t *testing.T) {
	t.Parallel()
	var closes atomic.Int32
	g := New(func() error { closes.Add(1); return nil })
	ctx, done, err := g.Begin(t.Context())
	require.NoError(t, err)
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	err = g.Close(expired)
	var drainErr *DrainError
	require.ErrorAs(t, err, &drainErr)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.Zero(t, closes.Load())
	_, _, err = g.Begin(t.Context())
	require.ErrorIs(t, err, ErrClosing)
	done()
	done()
	require.NoError(t, drainErr.Retry(t.Context()))
	require.NoError(t, g.Close(t.Context()))
	assert.EqualValues(t, 1, closes.Load())
}

func TestDrainJoinsLateFinalization(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	g := New(func() error { close(started); <-release; return nil })
	g.Fence()
	<-started
	expired, cancel := context.WithCancel(t.Context())
	cancel()
	var drainErr *DrainError
	require.ErrorAs(t, g.Close(expired), &drainErr)
	close(release)
	require.NoError(t, drainErr.Retry(t.Context()))
}

func TestDrainRetriesFailedStopWithoutClosingStore(t *testing.T) {
	t.Parallel()
	stopErr := errors.New("stop tools failed")
	var stops, closes atomic.Int32
	g := New(func() error {
		if stops.Add(1) == 1 {
			return stopErr
		}
		closes.Add(1)
		return nil
	})
	var drainErr *DrainError
	require.ErrorAs(t, g.Close(t.Context()), &drainErr)
	require.ErrorIs(t, drainErr, stopErr)
	assert.Zero(t, closes.Load(), "failed tool stop must retain the store")
	require.NoError(t, drainErr.Retry(t.Context()))
	require.NoError(t, g.Close(t.Context()))
	assert.EqualValues(t, 2, stops.Load())
	assert.EqualValues(t, 1, closes.Load())
}

func TestDrainDeadlineRetainsOwnershipUntilRetry(t *testing.T) {
	t.Parallel()
	var closes atomic.Int32
	g := New(func() error { closes.Add(1); return nil })
	invocationCtx, done, err := g.Begin(t.Context())
	require.NoError(t, err)
	deadline, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	var drainErr *DrainError
	require.ErrorAs(t, g.Close(deadline), &drainErr)
	require.ErrorIs(t, drainErr, context.DeadlineExceeded)
	require.ErrorIs(t, invocationCtx.Err(), context.Canceled)
	assert.Zero(t, closes.Load())
	_, _, err = g.Begin(t.Context())
	require.ErrorIs(t, err, ErrClosing)
	done()
	require.NoError(t, drainErr.Retry(t.Context()))
	require.NoError(t, g.Close(t.Context()))
	assert.EqualValues(t, 1, closes.Load())
}
