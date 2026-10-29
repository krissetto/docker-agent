package sidebar

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Two instances of the same agent keep distinct session usage; the inspector
// reads the subagent row's own session, never the agent-name aggregate.
func TestSessionUsageKeepsSameAgentInstancesDistinct(t *testing.T) {
	m := newSubagentTestModel(t)
	recordAgentUsage(m, "session-a", "worker", 30_000, 100_000)
	recordAgentUsage(m, "session-b", "worker", 80_000, 100_000)

	a, okA := m.SessionUsage("session-a")
	b, okB := m.SessionUsage("session-b")
	require.True(t, okA)
	require.True(t, okB)
	require.Equal(t, int64(30_000), a.ContextLength)
	require.Equal(t, int64(80_000), b.ContextLength)

	_, ok := m.SessionUsage("session-missing")
	require.False(t, ok, "an unknown session has no usage, so the inspector shows UNKNOWN")
}
