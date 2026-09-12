package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubagentsValidReferences(t *testing.T) {
	t.Parallel()

	cfg := `version: "16"
agents:
  root:
    model: openai/gpt-4o
    subagents:
      - worker
      - agent: researcher
        name: web
        description: Web research
  worker:
    model: openai/gpt-4o
  researcher:
    model: openai/gpt-4o
`
	loaded, err := Load(t.Context(), NewBytesSource("test", []byte(cfg)))
	require.NoError(t, err)
	require.NotNil(t, loaded)

	root, ok := loaded.Agents.Lookup("root")
	require.True(t, ok)
	require.Len(t, root.Subagents, 2)
	assert.Equal(t, "worker", root.Subagents[0].Agent)
	assert.Equal(t, "web", root.Subagents[1].ResolvedName())
}

func TestSubagentsRejectsUnknownAgent(t *testing.T) {
	t.Parallel()

	cfg := `version: "16"
agents:
  root:
    model: openai/gpt-4o
    subagents:
      - ghost
`
	_, err := Load(t.Context(), NewBytesSource("test", []byte(cfg)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-existent async subagent 'ghost'")
}

func TestSubagentsRejectsDuplicateAlias(t *testing.T) {
	t.Parallel()

	cfg := `version: "16"
agents:
  root:
    model: openai/gpt-4o
    subagents:
      - agent: worker
        name: dup
      - agent: researcher
        name: dup
  worker:
    model: openai/gpt-4o
  researcher:
    model: openai/gpt-4o
`
	_, err := Load(t.Context(), NewBytesSource("test", []byte(cfg)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate async subagent alias 'dup'")
}

func TestSubagentsRejectsExternalReference(t *testing.T) {
	t.Parallel()

	cfg := `version: "16"
agents:
  root:
    model: openai/gpt-4o
    subagents:
      - agentcatalog/review-pr
`
	_, err := Load(t.Context(), NewBytesSource("test", []byte(cfg)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "external async subagent")
}
