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

func TestGetToolsForAgent_ToolsetReadOnly(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Instruction: "test",
		Toolsets: []latest.Toolset{
			{Type: "background_jobs", ReadOnly: true},
		},
	}

	runConfig := config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: &noEnvProvider{},
	}
	expander := js.NewJsExpander(runConfig.EnvProvider())

	got, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "test-config", &loadOptions{toolsetRegistry: testToolsetRegistry()}, expander)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, got, 1)

	names := toolNames(t, got[0])
	assert.ElementsMatch(t, []string{"list_background_jobs", "view_background_job", "wait_background_job"}, names)
}

func TestGetToolsForAgent_AgentReadOnly(t *testing.T) {
	t.Parallel()

	// The agent-level flag applies the read-only filter to every toolset,
	// even though the toolset itself does not set readonly.
	a := &latest.AgentConfig{
		Instruction: "test",
		ReadOnly:    true,
		Toolsets: []latest.Toolset{
			{Type: "background_jobs"},
		},
	}

	runConfig := config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: &noEnvProvider{},
	}
	expander := js.NewJsExpander(runConfig.EnvProvider())

	got, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "test-config", &loadOptions{toolsetRegistry: testToolsetRegistry()}, expander)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, got, 1)

	names := toolNames(t, got[0])
	assert.ElementsMatch(t, []string{"list_background_jobs", "view_background_job", "wait_background_job"}, names)
}

func TestGetToolsForAgent_NoReadOnlyKeepsAllTools(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Instruction: "test",
		Toolsets: []latest.Toolset{
			{Type: "shell"},
			{Type: "background_jobs"},
		},
	}

	runConfig := config.RuntimeConfig{
		Config:              config.Config{WorkingDir: t.TempDir()},
		EnvProviderForTests: &noEnvProvider{},
	}
	expander := js.NewJsExpander(runConfig.EnvProvider())

	got, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "test-config", &loadOptions{toolsetRegistry: testToolsetRegistry()}, expander)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, got, 2)

	shellNames := toolNames(t, got[0])
	assert.ElementsMatch(t, []string{"shell"}, shellNames)

	backgroundNames := toolNames(t, got[1])
	assert.ElementsMatch(t, []string{
		"run_background_job", "list_background_jobs",
		"view_background_job", "stop_background_job", "wait_background_job",
	}, backgroundNames)
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
				Toolsets:      []latest.Toolset{{Type: "background_jobs", Defer: latest.DeferConfig{DeferAll: true}}},
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
			for _, forbidden := range []string{subagent.ToolSpawnSubagent, subagent.ToolSendMessage, subagent.ToolStopSubagent, "transfer_task", "handoff", "run_background_job"} {
				assert.NotContains(t, namesOf(all), forbidden)
			}
			if codeMode {
				assert.Contains(t, namesOf(all), "run_tools_with_javascript")
				output := callTool(t, all, "run_tools_with_javascript", codemode.RunToolsWithJavascriptArgs{Script: "return typeof spawn_subagent + ':' + typeof transfer_task + ':' + typeof run_background_job;"})
				assert.Contains(t, output, "undefined:undefined:undefined")
			} else {
				assert.Contains(t, namesOf(all), subagent.ToolReadSubagent)
				search := callTool(t, all, deferred.ToolNameSearchTool, deferred.SearchToolArgs{Query: "background"})
				assert.Contains(t, search, "list_background_jobs")
				assert.NotContains(t, search, "run_background_job")
				callTool(t, all, deferred.ToolNameAddTool, deferred.AddToolArgs{Name: "list_background_jobs"})
				all, err = ag.Tools(t.Context())
				require.NoError(t, err)
				assert.Contains(t, namesOf(all), "list_background_jobs")
			}
		})
	}
}
