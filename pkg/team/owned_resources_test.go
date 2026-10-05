package team

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/tools"
)

type retryStopToolset struct {
	closes  atomic.Int32
	stops   atomic.Int32
	failure error
	entered chan struct{}
	release chan struct{}
}

func (*retryStopToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (s *retryStopToolset) Start(context.Context) error {
	if s.entered != nil {
		close(s.entered)
		<-s.release
	}
	return nil
}

func (s *retryStopToolset) Stop(context.Context) error {
	if s.stops.Add(1) == 1 {
		return s.failure
	}
	return nil
}
func (s *retryStopToolset) Close() error { s.closes.Add(1); return nil }

func TestOwnedResourcesWaitForSuccessfulStopRetry(t *testing.T) {
	t.Parallel()
	inner := &retryStopToolset{failure: errors.New("stop failed")}
	a := agent.New("root", "", agent.WithToolSets(inner))
	tm := New(WithAgents(a), WithOwnedResources(inner))
	ts, ok := tools.As[*tools.StartableToolSet](a.ToolSets()[0])
	require.True(t, ok)
	require.NoError(t, ts.Start(t.Context()))
	require.ErrorIs(t, tm.StopToolSets(t.Context()), inner.failure)
	require.Zero(t, inner.closes.Load())
	require.True(t, ts.IsStarted())
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { require.NoError(t, tm.StopToolSets(t.Context())) })
	}
	wg.Wait()
	require.EqualValues(t, 2, inner.stops.Load())
	require.EqualValues(t, 1, inner.closes.Load())
	require.False(t, ts.IsStarted())
}

func TestOwnedResourcesRetainedWhileStartRemainsInFlight(t *testing.T) {
	t.Parallel()
	inner := &retryStopToolset{entered: make(chan struct{}), release: make(chan struct{})}
	a := agent.New("root", "", agent.WithToolSets(inner))
	tm := New(WithAgents(a), WithOwnedResources(inner))
	ts, ok := tools.As[*tools.StartableToolSet](a.ToolSets()[0])
	require.True(t, ok)
	released := sync.OnceFunc(func() { close(inner.release) })
	t.Cleanup(released)
	started := make(chan error, 1)
	go func() { started <- ts.Start(t.Context()) }()
	<-inner.entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, tm.StopToolSets(ctx), context.Canceled)
	require.ErrorIs(t, tm.StopToolSets(ctx), context.Canceled, "retry cannot declare a live Start settled")
	require.Zero(t, inner.closes.Load())
	require.Zero(t, inner.stops.Load())
	released()
	select {
	case err := <-started:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("late Start did not settle")
	}
	require.NoError(t, tm.StopToolSets(t.Context()))
	require.EqualValues(t, 1, inner.stops.Load())
	require.EqualValues(t, 1, inner.closes.Load())
}
