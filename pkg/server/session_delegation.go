package server

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/runtime"
)

func (s *Server) canonicalDelegationPolicy(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	controller, ok := handle.(runtime.SessionDelegationController)
	if !ok || !handle.Metadata().Capabilities.DelegationPolicy {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "delegation_policy"))
	}
	ctx := c.Request().Context()
	if c.Request().Method == http.MethodPatch {
		var request struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeSessionJSON(c, &request); err != nil || request.Enabled == nil {
			return sessionRequestError("enabled is required")
		}
		if err := controller.SetDelegationPolicy(ctx, *request.Enabled); err != nil {
			return sessionHTTPError(err)
		}
	}
	enabled, err := controller.DelegationPolicy(ctx)
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"enabled": enabled})
}
