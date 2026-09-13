package lifecycle

import (
	"context"
	"errors"
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
	owner *testSupervisor
	stops int
}

func (*testToolSet) Start(context.Context) error { return nil }
func (s *testToolSet) Stop(context.Context) error {
	if !s.owner.drained.Load() {
		return errors.New("tools stopped before runtime drained")
	}
	s.stops++
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
