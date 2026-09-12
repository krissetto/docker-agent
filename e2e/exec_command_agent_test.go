package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExec_CommandTargetsAgent verifies that the specialist can be selected
// directly as the session's immutable binding. Session-v2 deliberately rejects
// changing a live root session's identity through an agent-targeting command.
func TestExec_CommandTargetsAgent(t *testing.T) {
	t.Parallel()
	out := runCLI(t, "run", "--exec", "--agent", "specialist", "testdata/command_agent.yaml", "What's 2+2?")

	require.Equal(t, "SPECIALIST: 4", out)
}
