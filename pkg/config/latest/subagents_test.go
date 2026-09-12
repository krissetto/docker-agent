package latest

import (
	"encoding/json"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubagentRefUnmarshalYAMLString(t *testing.T) {
	var refs SubagentRefs
	require.NoError(t, yaml.Unmarshal([]byte("- worker\n"), &refs))
	require.Len(t, refs, 1)
	assert.Equal(t, "worker", refs[0].Agent)
	assert.Equal(t, "worker", refs[0].ResolvedName())
}

func TestSubagentRefUnmarshalYAMLObject(t *testing.T) {
	var refs SubagentRefs
	require.NoError(t, yaml.Unmarshal([]byte("- agent: researcher\n  name: web\n  description: Web research\n"), &refs))
	require.Len(t, refs, 1)
	assert.Equal(t, "researcher", refs[0].Agent)
	assert.Equal(t, "web", refs[0].Name)
	assert.Equal(t, "Web research", refs[0].Description)
	assert.Equal(t, "web", refs[0].ResolvedName())
}

func TestSubagentRefUnmarshalJSONStringAndObject(t *testing.T) {
	var refs SubagentRefs
	require.NoError(t, json.Unmarshal([]byte(`["worker", {"agent":"researcher", "name":"web"}]`), &refs))
	require.Len(t, refs, 2)
	assert.Equal(t, "worker", refs[0].Agent)
	assert.Equal(t, "researcher", refs[1].Agent)
	assert.Equal(t, "web", refs[1].ResolvedName())
}

func TestSubagentRefRejectsEmptyAgent(t *testing.T) {
	var refs SubagentRefs
	assert.Error(t, yaml.Unmarshal([]byte("- agent: ''\n"), &refs))
}

func TestSubagentRefsAgentNamesDeduplicates(t *testing.T) {
	refs := SubagentRefs{{Agent: "a"}, {Agent: "b"}, {Agent: "a", Name: "a2"}}
	assert.Equal(t, []string{"a", "b"}, refs.AgentNames())
}
