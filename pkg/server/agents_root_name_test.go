package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestAgentsAPIEntryKeepsSourceAndRootAgentIdentitiesSeparate(t *testing.T) {
	entry, ok := agentsAPIEntry("workspace-source.yaml", &latest.Config{Agents: latest.Agents{{Name: "planner", Description: "The actual configured root"}, {Name: "worker"}}})
	require.True(t, ok)
	require.Equal(t, "workspace-source.yaml", entry.Name)
	require.Equal(t, "planner", entry.RootAgentName)
	require.True(t, entry.Multi)
	encoded, err := json.Marshal(entry)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"root_agent_name":"planner"`)
}
