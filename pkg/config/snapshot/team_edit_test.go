package snapshot_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/snapshot"
)

const snapshotTeamYAML = `version: "16"
agents:
  root:
    model: openai/test
    instruction: test
`

func TestTeamEditRuntimeGrantSnapshotRoundTripAndDigest(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	base, err := snapshot.Snapshot(t.Context(), config.NewBytesSource("team.yaml", []byte(snapshotTeamYAML)), snapshot.Options{Startup: snapshot.Startup{Workspace: workspace}})
	require.NoError(t, err)
	baseDigest, err := base.Digest()
	require.NoError(t, err)
	// Canonical default-off mode and nil allowlist retain the pre-feature JSON shape.
	startupBytes, err := json.Marshal(base.Startup())
	require.NoError(t, err)
	assert.NotContains(t, string(startupBytes), "team_edit_mode")
	assert.NotContains(t, string(startupBytes), "team_edit_allowed_agent_ids")

	wideStartup := snapshot.Startup{Workspace: workspace, Runtime: config.Config{TeamEditMode: "full"}}
	wide, err := snapshot.Snapshot(t.Context(), config.NewBytesSource("team.yaml", []byte(snapshotTeamYAML)), snapshot.Options{Startup: wideStartup})
	require.NoError(t, err)
	wideBytes, err := wide.Marshal()
	require.NoError(t, err)
	parsed, err := snapshot.Parse(wideBytes)
	require.NoError(t, err)
	assert.Equal(t, "full", parsed.Startup().Runtime.TeamEditMode)
	wideDigest, err := wide.Digest()
	require.NoError(t, err)
	assert.NotEqual(t, baseDigest, wideDigest, "trusted authority is part of startup identity")

	denyAllStartup := snapshot.Startup{Workspace: workspace, Runtime: config.Config{TeamEditMode: "restricted", TeamEditAllowedAgentIDs: []string{}, TeamEditAllowedAgentIDsSet: true}}
	denyAll, err := snapshot.Snapshot(t.Context(), config.NewBytesSource("team.yaml", []byte(snapshotTeamYAML)), snapshot.Options{Startup: denyAllStartup})
	require.NoError(t, err)
	denyBytes, err := denyAll.Marshal()
	require.NoError(t, err)
	parsed, err = snapshot.Parse(denyBytes)
	require.NoError(t, err)
	assert.Equal(t, "restricted", parsed.Startup().Runtime.TeamEditMode)
	assert.True(t, parsed.Startup().Runtime.TeamEditAllowedAgentIDsSet)
	grant := parsed.Startup().Runtime.TeamEditGrant()
	assert.True(t, grant.AllowedAgentIDsSet)
	assert.NotNil(t, grant.AllowedAgentIDs)
	assert.Empty(t, grant.AllowedAgentIDs)
}
