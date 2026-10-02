package server

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

// WithServerInfo supplies immutable managed-launch identity, never client input.
// Version, supported capabilities and readiness remain server-owned.
func WithServerInfo(info api.ServerInfo) Option {
	return func(o *serverOptions) { o.serverInfo = info }
}
func newServerInfo(info api.ServerInfo) api.ServerInfo {
	info.Version = 1
	info.SessionAPIVersion = api.SessionAPIVersion
	if info.InstanceID == "" {
		info.InstanceID = uuid.NewString()
	}
	info.Capabilities = []string{"session_v2", "session_tools", "session_permissions", "mcp_prompts", "todo_editing", "session_branching", "session_explicit_ids"}
	return info
}
func (s *Server) serverIdentity(c echo.Context) error {
	info := s.serverInfo
	info.Capabilities = slices.Clone(info.Capabilities)
	// Only an explicit bounded storage probe can establish readiness. Generic
	// adapters without it remain identifiable but not ready; never fall back to
	// enumerating history or hydrating agents, providers, or session handles.
	ctx, cancel := context.WithTimeout(c.Request().Context(), time.Second)
	defer cancel()
	info.Ready = false
	if probe, ok := s.sm.sessionStore.(session.StorePinger); ok {
		info.Ready = probe.Ping(ctx) == nil
	}
	return c.JSON(http.StatusOK, info)
}
