package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/subagent"
	toolspkg "github.com/docker/docker-agent/pkg/tools"
)

func TestGetToolsForAgent_SubagentsInjectsAsyncTools(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Name:      "root",
		Subagents: latest.SubagentRefs{{Agent: "worker", Description: "Does work"}},
	}
	runConfig := config.RuntimeConfig{EnvProviderForTests: &noEnvProvider{}}
	expander := js.NewJsExpander(runConfig.EnvProvider())

	toolSets, warnings, err := getToolsForAgent(t.Context(), a, ".", &runConfig, "test-config", &loadOptions{toolsetRegistry: &toolsetRegistry{}}, expander)
	require.NoError(t, err)
	require.Empty(t, warnings)
	require.Len(t, toolSets, 1)

	toolDefs, err := toolSets[0].Tools(t.Context())
	require.NoError(t, err)
	var names []string
	for _, tool := range toolDefs {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{subagent.ToolSpawnSubagent, subagent.ToolSendMessage, subagent.ToolReadSubagent, subagent.ToolStopSubagent}, names)
	assert.Empty(t, toolspkg.GetInstructions(toolSets[0]), "subagent harness prompt is injected as core system prompt, not toolset instructions")
}

func TestLoadWithConfig_WarnsWhenAgentMixesDelegationModes(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: coordinate
    sub_agents: [worker]
    subagents: [worker]
  worker:
    model: openai/gpt-4o
    instruction: work
`)

	result, err := LoadWithConfig(t.Context(), config.NewBytesSource("mixed-subagents.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)
	root, err := result.Team.Agent("root")
	require.NoError(t, err)
	warnings := root.DrainWarnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "sub_agents")
	assert.Contains(t, warnings[0], "subagents")
	assert.Contains(t, warnings[0], "synchronously with transfer_task")
	assert.Contains(t, warnings[0], "asynchronously")
	assert.Contains(t, warnings[0], "avoid declaring both")
	assert.Len(t, root.AsyncSubagents(), 1, "warning must not reject async configuration")
}

func TestLoadWithConfig_SubagentsAreAvailableOnAgent(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: coordinate
    subagents:
      - agent: worker
        name: work
        description: Does work
  worker:
    model: openai/gpt-4o
    instruction: do work
`)

	result, err := LoadWithConfig(t.Context(), config.NewBytesSource("async-subagents.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	root, err := result.Team.Agent("root")
	require.NoError(t, err)
	require.Len(t, root.AsyncSubagents(), 1)
	assert.Equal(t, "worker", root.AsyncSubagents()[0].Agent)
	assert.Equal(t, "work", root.AsyncSubagents()[0].Name)
	assert.Contains(t, root.AsyncHarnessPrompt(), "# Async subagents")
	assert.Contains(t, root.AsyncHarnessPrompt(), "- work: Does work")

	worker, err := result.Team.Agent("worker")
	require.NoError(t, err)
	assert.Empty(t, worker.AsyncHarnessPrompt())
}
