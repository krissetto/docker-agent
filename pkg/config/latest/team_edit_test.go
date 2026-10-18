package latest_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestTeamEditToolsetIsSchemaValidWithoutGrantField(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load(t.Context(), config.NewBytesSource("team.yaml", []byte(`agents:
  root:
    model: openai/test
    toolsets:
      - type: team_edit
    instruction: Edit only with the externally supplied runtime grant.
`)))
	require.NoError(t, err)
	require.Equal(t, latest.Version, cfg.Version)
	require.Len(t, cfg.Agents[0].Toolsets, 1)
	require.Equal(t, "team_edit", cfg.Agents[0].Toolsets[0].Type)
}
