package server

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/runtime"
)

func (s *Server) registerGeneratedMediaRoutes(group *echo.Group) {
	group.POST("/:id/generated-media", s.canonicalGeneratedMedia)
}

func (s *Server) canonicalGeneratedMedia(c echo.Context) error {
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, 16<<10)
	var ref runtime.GeneratedFileRef
	if err := decodeSessionJSON(c, &ref); err != nil {
		return sessionRequestError("invalid generated media reference")
	}
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	resolver, ok := handle.(runtime.GeneratedFileResolver)
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "generated media unavailable")
	}
	resolved, err := resolver.ResolveGeneratedFile(c.Request().Context(), ref)
	if err != nil || resolved == nil || len(resolved.Data) > runtime.MaxGeneratedFileBytes {
		return echo.NewHTTPError(http.StatusNotFound, "generated media unavailable")
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("X-Content-Type-Options", "nosniff")
	return c.Blob(http.StatusOK, "application/octet-stream", resolved.Data)
}
