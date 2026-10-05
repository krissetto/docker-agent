package server

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/runtime"
)

func (s *Server) canonicalSessionAgentConfig(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	reader, ok := handle.(runtime.SessionAgentConfigReader)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "agent_config"))
	}
	name := c.QueryParam("agent")
	if name == "" {
		return sessionRequestError("agent name is required")
	}
	info, err := reader.SessionAgentConfig(c.Request().Context(), name)
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, info)
}
