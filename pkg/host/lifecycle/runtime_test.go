package lifecycle

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type testSupervisor struct {
	runtime.SessionRuntimeSupervisor

	calls   int
	err     error
	drained atomic.Bool
}

func (s *testSupervisor) Runtime() runtime.SessionRuntime { return nil }
func (s *testSupervisor) Shutdown(context.Context) error {
	s.calls++
	if s.err == nil {
		s.drained.Store(true)
	}
	return s.err
}

type testToolSet struct {
	owner   *testSupervisor
	stops   int
	stopErr error
	closes  int
}

func (*testToolSet) Start(context.Context) error { return nil }
func (s *testToolSet) Stop(context.Context) error {
	if !s.owner.drained.Load() {
		return errors.New("tools stopped before runtime drained")
	}
	s.stops++
	if s.stops == 1 {
		return s.stopErr
	}
	return nil
}
func (*testToolSet) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }

func TestOwnedRuntimeDrainsBeforeStoppingToolsAndCanRetry(t *testing.T) {
	t.Parallel()
	owner := &testSupervisor{err: context.DeadlineExceeded}
	tool := &testToolSet{owner: owner}
	root := agent.New("root", "", agent.WithToolSets(tool))
	for _, ts := range root.ToolSets() {
		require.NoError(t, ts.(*tools.StartableToolSet).Start(t.Context()))
	}
	supervisor := OwnRuntime(owner, team.New(team.WithAgents(root)))
	require.ErrorIs(t, supervisor.Shutdown(t.Context()), context.DeadlineExceeded)
	assert.Zero(t, tool.stops)
	owner.err = nil
	require.NoError(t, supervisor.Shutdown(t.Context()))
	require.NoError(t, supervisor.Shutdown(t.Context()))
	assert.Equal(t, 1, tool.stops)
	assert.Equal(t, 2, owner.calls)
}

func (s *testToolSet) Close() error {
	s.closes++
	return nil
}

func TestOwnedRuntimeRetriesFailedToolStopBeforeClosingResources(t *testing.T) {
	t.Parallel()
	owner := &testSupervisor{}
	failure := errors.New("stop failed")
	tool := &testToolSet{owner: owner, stopErr: failure}
	root := agent.New("root", "", agent.WithToolSets(tool))
	for _, ts := range root.ToolSets() {
		require.NoError(t, ts.(*tools.StartableToolSet).Start(t.Context()))
	}
	supervisor := OwnRuntime(owner, team.New(team.WithAgents(root), team.WithOwnedResources(tool)))
	require.ErrorIs(t, supervisor.Shutdown(t.Context()), failure)
	require.Zero(t, tool.closes)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { require.NoError(t, supervisor.Shutdown(t.Context())) })
	}
	wg.Wait()
	require.Equal(t, 2, tool.stops)
	require.Equal(t, 1, tool.closes)
	require.Equal(t, 1, owner.calls, "retry final tool cleanup without draining runtime again")
}
