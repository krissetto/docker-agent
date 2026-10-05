package agent

import (
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNewToolSet_ReturnsFourTools(t *testing.T) {
	t.Parallel()
	ts := New()
	toolsList, err := ts.Tools(t.Context())
	require.NoError(t, err)
	assert.Len(t, toolsList, 4)

	names := make([]string, len(toolsList))
	for i, tl := range toolsList {
		names[i] = tl.Name
	}
	assert.Contains(t, names, ToolNameRunBackgroundAgent)
	assert.Contains(t, names, ToolNameListBackgroundAgents)
	assert.Contains(t, names, ToolNameViewBackgroundAgent)
	assert.Contains(t, names, ToolNameStopBackgroundAgent)
}

func TestNewToolSet_Instructions(t *testing.T) {
	t.Parallel()
	ts := New()
	instructable, ok := ts.(tools.Instructable)
	require.True(t, ok, "NewToolSet should implement Instructable")

	instructions := instructable.Instructions()
	assert.NotEmpty(t, instructions)
	assert.Contains(t, instructions, "run_background_agent")
	assert.Contains(t, instructions, "list_background_agents")
	assert.Contains(t, instructions, "view_background_agent")
	assert.Contains(t, instructions, "stop_background_agent")
}
