package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// RemoteClient is source-wide metadata only. Session APIs live on
// SessionTransport/SessionHandle and must not be added here.
type RemoteClient interface {
	GetAgent(ctx context.Context, id string) (*latest.Config, error)
}

var _ RemoteClient = (*Client)(nil)
