package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

// SessionCost is the per-session read the inspector uses for a subagent row.
func TestSessionCostReadsTheRecordedSessionOnly(t *testing.T) {
	s := &SessionState{}
	s.SetAgentUsage("session-a", "worker", runtime.Usage{Cost: 1.5})
	s.SetAgentUsage("session-b", "worker", runtime.Usage{Cost: 4})

	cost, ok := s.SessionCost("session-a")
	require.True(t, ok)
	require.InDelta(t, 1.5, cost, 1e-9)

	_, ok = s.SessionCost("session-missing")
	require.False(t, ok)
}
