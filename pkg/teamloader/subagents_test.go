package teamloader

import (
	"testing"

	"github.com/docker/docker-agent/pkg/environment"

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
	assert.ElementsMatch(t, []string{subagent.ToolSpawnSubagent, subagent.ToolSendMessage, subagent.ToolReadSubagent, subagent.ToolStopSubagent, subagent.ToolListAgents}, names)
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
    subagent_access:
      list_agents: tree
      send_message: tree
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
	captured, ok := result.Team.AgentConfig("root")
	require.True(t, ok)
	require.NotNil(t, captured.SubagentAccess)
	assert.Equal(t, latest.SubagentAccessTree, captured.SubagentAccess.ListAgents)
	assert.Equal(t, latest.SubagentAccessTree, captured.SubagentAccess.SendMessage)
	assert.Contains(t, root.AsyncHarnessPrompt(), "# Async subagents")
	assert.NotContains(t, root.AsyncHarnessPrompt(), "- work: Does work")
	assert.NotContains(t, root.AsyncHarnessPrompt(), "Your subagents")

	worker, err := result.Team.Agent("worker")
	require.NoError(t, err)
	assert.Empty(t, worker.AsyncHarnessPrompt())
}

func TestLoadRetainsCredentialSafeAuthoredDefinitionSnapshot(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "should-not-be-captured")
	t.Setenv("MODEL_NAME", "gpt-4o")
	t.Setenv("INSTRUCTION_SUFFIX", "private-expanded-instruction")
	data := []byte(`id: research-team
models:
  primary:
    provider: openai
    model: ${env.MODEL_NAME}
    base_url: https://user:secret@example.invalid/v1?token=secret
agents:
  root:
    id: coordinator
    model: primary
    description: Coordinates
    instruction: "Keep the original authored instructions: ${env.INSTRUCTION_SUFFIX}"
    toolsets:
      - type: mcp
        name: private-server
        command: secret-command
        env: {TOKEN: literal-secret}
        instruction: Search the repository
  worker:
    id: worker-id
    model: openai/gpt-4o
    description: Researches
`)
	result, err := LoadWithConfig(t.Context(), config.NewBytesSource("team.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)
	snapshot := result.Definitions
	assert.Equal(t, "research-team", snapshot.TeamID)
	entry, ok := snapshot.AgentDefinition("root")
	require.True(t, ok)
	assert.Equal(t, "agent", entry.Kind)
	text := string(entry.Payload)
	assert.Contains(t, text, "Keep the original authored instructions: ${env.INSTRUCTION_SUFFIX}")
	assert.Contains(t, text, "Search the repository")
	assert.NotContains(t, text, "secret-command")
	assert.NotContains(t, text, "literal-secret")
	assert.NotContains(t, text, "user:secret")
	assert.NotContains(t, text, "token=secret")
	assert.NotContains(t, text, "should-not-be-captured")
	assert.NotContains(t, text, "private-expanded-instruction")
	assert.NotContains(t, text, "${env.MODEL_NAME}")
	teamEntry := snapshot.TeamDefinitionEntry()
	assert.Equal(t, snapshot.TeamDigest, teamEntry.Digest)
	assert.NotContains(t, string(teamEntry.Payload), "Keep the original")
	assert.NotContains(t, string(teamEntry.Payload), "secret-command")
	assert.Equal(t, snapshot.TeamDigest, result.Team.DefinitionSnapshot().TeamDigest)
	mutated := entry.Payload
	mutated[0] = 'X'
	again, ok := snapshot.AgentDefinition("root")
	require.True(t, ok)
	assert.NotEqual(t, mutated[0], again.Payload[0], "definition snapshots return independent bytes")
}

func TestDefinitionSnapshotIncludesExternalAgentWithSanitizedReference(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "not-captured")
	root := []byte(`
id: integration-team
agents:
  root:
    model: openai/gpt-4o
    instruction: coordinator instructions
    sub_agents:
      - "reviewer:https://user:secret@example.invalid/review.yaml?access_token=credential"
`)
	external := []byte(`agents:
  root:
    model: openai/gpt-4o
    description: External reviewer
    instruction: Review the requested changes.
`)
	result, err := LoadWithConfig(t.Context(), config.NewBytesSource("root.yaml", root), &config.RuntimeConfig{}, withTestProviderRegistry(WithSourceResolver(func(ref string, _ environment.Provider) (config.Source, error) {
		assert.Equal(t, "https://user:secret@example.invalid/review.yaml?access_token=credential", ref)
		return config.NewBytesSource(ref, external), nil
	}))...)
	require.NoError(t, err)
	entry, ok := result.Definitions.AgentDefinition("reviewer")
	require.True(t, ok, "loaded external default must have an immutable definition row")
	text := string(entry.Payload)
	assert.Contains(t, text, "External reviewer")
	assert.Contains(t, text, "Review the requested changes.")
	assert.NotContains(t, text, "user:secret")
	assert.NotContains(t, text, "access_token=credential")
	assert.NotContains(t, text, "not-captured")
	assert.NotEmpty(t, entry.Digest)
}
