package teamloader

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

type ownedTestToolset struct {
	closes   atomic.Int32
	closeErr error
}

func (*ownedTestToolset) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (t *ownedTestToolset) Close() error                              { t.closes.Add(1); return t.closeErr }

func TestCreatorResourcesClosedByFinalTeamOwner(t *testing.T) {
	t.Parallel()
	resource := &ownedTestToolset{}
	registry := NewToolsetRegistry(map[string]ToolsetCreator{"memory": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
		return resource, nil
	}})
	source := config.NewBytesSource("owned.yaml", []byte(`agents:
  root:
    harness:
      type: codex
    toolsets:
      - type: memory
`))
	tm, err := Load(t.Context(), source, &config.RuntimeConfig{}, WithToolsetRegistry(registry))
	require.NoError(t, err)
	root, err := tm.DefaultAgent()
	require.NoError(t, err)
	require.NoError(t, root.StopToolSets(t.Context()))
	require.Zero(t, resource.closes.Load(), "session retirement must not close creator-owned resources")
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() { require.NoError(t, tm.StopToolSets(t.Context())) })
	}
	wg.Wait()
	require.EqualValues(t, 1, resource.closes.Load())
}

func TestLoadFailureClosesCreatorResources(t *testing.T) {
	t.Parallel()
	resource := &ownedTestToolset{closeErr: errors.New("close failed")}
	registry := NewToolsetRegistry(map[string]ToolsetCreator{"memory": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
		return resource, nil
	}})
	source := config.NewBytesSource("owned.yaml", []byte(`agents:
  root:
    harness:
      type: codex
    code_mode_tools: true
    toolsets:
      - type: memory
`))
	tm, err := Load(t.Context(), source, &config.RuntimeConfig{}, WithToolsetRegistry(registry))
	require.Nil(t, tm)
	require.ErrorContains(t, err, "code_mode_tools needs")
	require.ErrorIs(t, err, resource.closeErr)
	require.EqualValues(t, 1, resource.closes.Load())
}
