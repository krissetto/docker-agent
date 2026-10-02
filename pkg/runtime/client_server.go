package runtime

import (
	"context"
	"fmt"
	"net/http"

	"github.com/docker/docker-agent/pkg/api"
)

func (c *Client) ServerInfo(ctx context.Context) (api.ServerInfo, error) {
	var info api.ServerInfo
	if err := c.sessionJSON(ctx, http.MethodGet, api.ServerInfoPath, nil, &info); err != nil {
		return info, err
	}
	if info.Version != 1 || info.SessionAPIVersion != api.SessionAPIVersion || info.InstanceID == "" {
		return api.ServerInfo{}, fmt.Errorf("unsupported or invalid server identity (version %d, session API %d)", info.Version, info.SessionAPIVersion)
	}
	return info, nil
}
