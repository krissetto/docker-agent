package server

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
)

// SourceRegistrar resolves a caller's selection to a validated immutable
// source. The registration route is protected by the server bearer middleware.
type SourceRegistrar func(context.Context, string, string) (api.SourceRegistration, config.Source, error)

func WithSourceRegistrar(registrar SourceRegistrar) Option {
	return func(o *serverOptions) { o.sourceRegistrar = registrar }
}

func (s *Server) registerSource(c echo.Context) error {
	if s.sourceRegistrar == nil {
		return echo.NewHTTPError(http.StatusNotFound, "source registration is unavailable")
	}
	var request api.SourceRegistrationRequest
	if err := decodeSessionJSON(c, &request); err != nil || request.Path == "" {
		return sessionRequestError("path is required")
	}
	registration, source, err := s.sourceRegistrar(c.Request().Context(), request.Path, request.WorkingDir)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if registration.SourceID == "" || registration.Revision == "" || source == nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "source registrar returned an incomplete revision")
	}
	staged, isStaged := source.(interface {
		CommitSource() error
		AbortSource()
	})
	committed := false
	if isStaged {
		defer func() {
			if !committed {
				staged.AbortSource()
			}
		}()
	}
	commit := func() error {
		if isStaged {
			return staged.CommitSource()
		}
		return nil
	}
	if err := s.sm.PublishSourceWithCommit(registration.SourceID, source, commit); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	committed = isStaged
	return c.JSON(http.StatusOK, registration)
}
