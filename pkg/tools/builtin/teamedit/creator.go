package teamedit

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// Creator validates the immutable startup grant. YAML and tool arguments can
// request the capability but cannot create the external grant themselves.
func Creator(_ context.Context, _ latest.Toolset, _ string, runtimeConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	if runtimeConfig == nil {
		return nil, errors.New("team_edit requires a trusted runtime grant")
	}
	if err := runtimeConfig.ValidateTeamEditGrant(); err != nil {
		return nil, err
	}
	// The authored declaration requests the capability, but startup off is
	// inert rather than an error so ordinary teams can safely include it.
	return &ToolSet{}, nil
}
