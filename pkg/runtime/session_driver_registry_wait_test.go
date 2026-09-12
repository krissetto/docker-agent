package runtime

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestReleaseTimeoutDoesNotLeaveWaiterGoroutine(t *testing.T) {
	g := newSessionDriverRegistry(nil)
	d := newSessionDriver(&LocalRuntime{}, session.New(session.WithID("release-timeout")))
	d.wg.Add(1)
	g.drivers[d.sessionID()] = d
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	require.ErrorIs(t, g.Release(ctx, d.sessionID()), context.DeadlineExceeded)
	runtime.Gosched()
	assert.LessOrEqual(t, runtime.NumGoroutine(), before+1)

	d.wg.Done()
	select {
	case <-d.Done():
	case <-time.After(time.Second):
		t.Fatal("driver work did not drain after timed-out release")
	}
}

func TestCloseTimeoutDoesNotLeaveWaiterGoroutinesAndCanFinish(t *testing.T) {
	g := newSessionDriverRegistry(nil)
	const drivers = 32
	all := make([]*sessionDriver, 0, drivers)
	for i := range drivers {
		d := newSessionDriver(&LocalRuntime{}, session.New(session.WithID(string(rune('a'+i)))))
		d.wg.Add(1)
		g.drivers[d.sessionID()] = d
		all = append(all, d)
	}
	before := runtime.NumGoroutine()
	for range 8 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		err := g.CloseContext(ctx)
		cancel()
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
	runtime.Gosched()
	assert.LessOrEqual(t, runtime.NumGoroutine(), before+1)

	for _, d := range all {
		d.wg.Done()
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, g.CloseContext(ctx))
	assert.Empty(t, g.drivers)
}
