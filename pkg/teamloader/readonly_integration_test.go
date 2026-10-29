package teamloader

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/deferred"
	"github.com/docker/docker-agent/pkg/tools/codemode"
	"io"
)

// toolNames returns the names of the tools exposed by ts.
func toolNames(t *testing.T, ts tools.ToolSet) []string {
	t.Helper()
	all, err := ts.Tools(t.Context())
	require.NoError(t, err)
	var names []string
	for _, tool := range all {
		names = append(names, tool.Name)
	}
	return names
}

func TestShellDiagnosticsRemainAvailableWithoutStandaloneProcessToolset(t *testing.T) {
	runConfig := config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}, EnvProviderForTests: &noEnvProvider{}}
	sets, warnings, err := getToolsForAgent(t.Context(), &latest.AgentConfig{Toolsets: []latest.Toolset{{Type: "shell"}}}, ".", &runConfig, "shell-only", &loadOptions{toolsetRegistry: testToolsetRegistry(), ownedResources: new([]io.Closer)}, js.NewJsExpander(runConfig.EnvProvider()))
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, sets, 1)
	assert.ElementsMatch(t, []string{"shell", "list_running_processes", "inspect_running_process", "kill_running_process"}, toolNames(t, sets[0]))
}

func TestGetToolsForAgent_ReadOnlyFinalComposition(t *testing.T) {
	t.Parallel()
	for _, codeMode := range []bool{false, true} {
		t.Run(fmt.Sprint("code mode=", codeMode), func(t *testing.T) {
			t.Parallel()
			a := &latest.AgentConfig{
				ReadOnly:      true,
				SubAgents:     []string{"worker"},
				Subagents:     latest.SubagentRefs{{Agent: "worker"}},
				Handoffs:      []string{"worker"},
				CodeModeTools: codeMode,
			}
			runConfig := config.RuntimeConfig{Config: config.Config{WorkingDir: t.TempDir()}, EnvProviderForTests: &noEnvProvider{}}
			sets, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "readonly", &loadOptions{
				toolsetRegistry: testToolsetRegistry(),
				newDeferred:     func() DeferredToolSet { return deferred.New() },
				codeMode:        codemode.Wrap,
			}, js.NewJsExpander(runConfig.EnvProvider()))
			require.NoError(t, err)
			require.Empty(t, warnings)
			ag := agent.New("root", "", agent.WithReadOnly(true), agent.WithToolSets(sets...))
			all, err := ag.Tools(t.Context())
			require.NoError(t, err)
			for _, tool := range all {
				assert.True(t, tool.Annotations.ReadOnlyHint, tool.Name)
			}
			for _, forbidden := range []string{subagent.ToolSpawnSubagent, subagent.ToolSendMessage, subagent.ToolStopSubagent, "transfer_task", "handoff", "list_running_processes"} {
				assert.NotContains(t, namesOf(all), forbidden)
			}
			if codeMode {
				assert.Contains(t, namesOf(all), "run_tools_with_javascript")
				output := callTool(t, all, "run_tools_with_javascript", codemode.RunToolsWithJavascriptArgs{Script: "return typeof spawn_subagent + ':' + typeof transfer_task + ':' + typeof legacy_run_tool;"})
				assert.Contains(t, output, "undefined:undefined:undefined")
			} else {
				assert.Contains(t, namesOf(all), subagent.ToolReadSubagent)
			}
		})
	}
}
