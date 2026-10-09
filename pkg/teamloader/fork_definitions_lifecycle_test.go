package teamloader

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

type forkDefinitionToolset struct {
	stops   atomic.Int32
	failure error
}

func (*forkDefinitionToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (*forkDefinitionToolset) Start(context.Context) error                 { return nil }
func (s *forkDefinitionToolset) Stop(context.Context) error {
	if s.stops.Add(1) == 1 {
		return s.failure
	}
	return nil
}

func TestForkDefinitionsHaveFinalTeamLifecycle(t *testing.T) {
	first := &forkDefinitionToolset{failure: errors.New("first stop failed")}
	second := &forkDefinitionToolset{}
	created := 0
	registry := NewToolsetRegistry(map[string]ToolsetCreator{"fake": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
		created++
		if created == 1 {
			return first, nil
		}
		return second, nil
	}})
	source := config.NewBytesSource("fork-owned.yaml", []byte(`toolsets:
  extra:
    type: fake
agents:
  root:
    harness:
      type: codex
    skills:
      - name: builder
        description: Build.
        context: fork
        toolsets: [extra, extra]
        instructions: Build.
`))
	tm, err := Load(t.Context(), source, &config.RuntimeConfig{}, WithToolsetRegistry(registry))
	require.NoError(t, err)
	root, err := tm.DefaultAgent()
	require.NoError(t, err)
	var skills *skillstool.ToolSet
	for _, ts := range root.ToolSets() {
		if s, ok := tools.As[*skillstool.ToolSet](ts); ok {
			skills = s
		}
	}
	require.NotNil(t, skills)
	prepared, result, err := skills.PrepareForkSubSession(t.Context(), skillstool.RunSkillArgs{Name: "builder"}, tools.NopRuntime{})
	require.NoError(t, err)
	require.Nil(t, result)
	require.Len(t, prepared.ToolSets, 2)
	for _, ts := range prepared.ToolSets {
		startable, ok := tools.As[tools.Startable](ts)
		require.True(t, ok)
		require.NoError(t, startable.Start(t.Context()))
	}
	require.NoError(t, root.StopToolSets(t.Context()))
	require.Zero(t, first.stops.Load())
	require.Zero(t, second.stops.Load())
	require.ErrorIs(t, tm.StopToolSets(t.Context()), first.failure)
	require.EqualValues(t, 1, second.stops.Load(), "failed definition must not abandon later definitions")
	require.NoError(t, tm.StopToolSets(t.Context()))
	require.NoError(t, tm.StopToolSets(t.Context()))
	require.EqualValues(t, 2, first.stops.Load(), "failed stop retries")
	require.EqualValues(t, 1, second.stops.Load(), "successful definition stops exactly once")
}
