package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/team"
)

func TestAgentBoundCommandLookupDoesNotUseMutableCurrentAgent(t *testing.T) {
	root := agent.New("root", "root", agent.WithModel(&mockProvider{id: "test/root"}), agent.WithCommands(types.Commands{"who": {Instruction: "root command"}}))
	worker := agent.New("worker", "worker", agent.WithModel(&mockProvider{id: "test/worker"}), agent.WithCommands(types.Commands{"who": {Instruction: "worker command"}}))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)))
	require.NoError(t, err)

	cmd, _, ok := LookupCommand(t.Context(), rt, "worker", "/who")
	require.True(t, ok)
	assert.Equal(t, "worker command", cmd.Instruction)
	assert.Equal(t, "root", rt.CurrentAgentName(t.Context()), "lookup cannot mutate current agent")
	assert.Equal(t, "worker command", ResolveCommand(t.Context(), rt, "worker", "/who"))
}
