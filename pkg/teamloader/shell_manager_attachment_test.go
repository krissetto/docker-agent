package teamloader

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tools/builtin/shellprocess"
)

// Compile-time guard: if AttachProcessManager drifts, the loader's type
// assertion would silently stop attaching the shared process manager.
var _ processManagerAttacher = (*builtinshell.ToolSet)(nil)

// shellAdapter is a custom-registry wrapper around the builtin shell. It must
// forward Unwrap so tools.As still reaches the shell toolset.
type shellAdapter struct {
	inner tools.ToolSet
}

func (a *shellAdapter) Tools(ctx context.Context) ([]tools.Tool, error) {
	return a.inner.Tools(ctx)
}

func (a *shellAdapter) Unwrap() tools.ToolSet {
	return a.inner
}

var shellManagerInspectionNames = []string{
	shellprocess.ToolNameListRunningProcesses,
	shellprocess.ToolNameInspectRunningProcess,
	shellprocess.ToolNameKillRunningProcess,
}

func TestShellProcessManagerAttachesOnceThroughCustomAdapter(t *testing.T) {
	t.Parallel()

	runConfig := config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}
	registry := NewToolsetRegistry(map[string]ToolsetCreator{
		"shell": func(ctx context.Context, toolset latest.Toolset, _ string, rc *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
			inner, err := builtinshell.CreateToolSet(ctx, toolset, rc)
			if err != nil {
				return nil, err
			}
			return &shellAdapter{inner: inner}, nil
		},
	})
	resources := new([]io.Closer)
	t.Cleanup(func() {
		for _, closer := range *resources {
			_ = closer.Close()
		}
	})
	opts := &loadOptions{toolsetRegistry: registry, ownedResources: resources}
	agent := &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "shell"}, {Type: "shell"}}}

	sets, _, err := getToolsForAgent(t.Context(), agent, ".", &runConfig, "team.yaml", opts, newEnvExpander(runConfig.EnvProvider()))
	require.NoError(t, err)
	require.Len(t, sets, 2)

	var managers int
	for _, resource := range *resources {
		if _, ok := resource.(*shellprocess.Manager); ok {
			managers++
		}
	}
	assert.Equal(t, 1, managers, "one shared process manager per agent")

	first, err := sets[0].Tools(t.Context())
	require.NoError(t, err)
	second, err := sets[1].Tools(t.Context())
	require.NoError(t, err)
	for _, name := range shellManagerInspectionNames {
		assert.Equal(t, 1, countToolName(first, name), "diagnostics on first shell: %s", name)
		assert.Equal(t, 0, countToolName(second, name), "no diagnostics on later shell: %s", name)
	}
}

func TestNoShellToolsetCreatesNoProcessManager(t *testing.T) {
	t.Parallel()

	runConfig := config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}}
	registry := NewToolsetRegistry(map[string]ToolsetCreator{
		"plain": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
			return plainSet{}, nil
		},
	})
	resources := new([]io.Closer)
	opts := &loadOptions{toolsetRegistry: registry, ownedResources: resources}
	agent := &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "plain"}}}

	_, _, err := getToolsForAgent(t.Context(), agent, ".", &runConfig, "team.yaml", opts, newEnvExpander(runConfig.EnvProvider()))
	require.NoError(t, err)
	assert.Empty(t, *resources)
}
