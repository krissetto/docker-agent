package server

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
)

func (s *Server) registerWorkspaceSnapshotRoutes(group *echo.Group) {
	group.GET("/:id/workspace-snapshots", s.canonicalWorkspaceSnapshots)
	group.POST("/:id/workspace-snapshots/undo", s.canonicalUndoWorkspaceSnapshot)
	group.POST("/:id/workspace-snapshots/reset", s.canonicalResetWorkspaceSnapshot)
}

func (s *Server) canonicalWorkspaceSnapshots(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability, ok := handle.(runtime.SessionWorkspaceSnapshots)
	if !ok || !handle.Metadata().Capabilities.Snapshots {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "workspace_snapshots"))
	}
	out, err := capability.WorkspaceSnapshots(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Server) canonicalUndoWorkspaceSnapshot(c echo.Context) error {
	return s.restoreWorkspaceSnapshot(c, false)
}

func (s *Server) canonicalResetWorkspaceSnapshot(c echo.Context) error {
	return s.restoreWorkspaceSnapshot(c, true)
}

func (s *Server) restoreWorkspaceSnapshot(c echo.Context, reset bool) error {
	var req api.WorkspaceSnapshotRequest
	if err := decodeSessionJSON(c, &req); err != nil || req.ExpectedProof == "" || req.Keep < 0 {
		return sessionRequestError("expected_proof and a nonnegative keep are required")
	}
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability, ok := handle.(runtime.SessionWorkspaceSnapshots)
	if !ok || !handle.Metadata().Capabilities.Snapshots {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "workspace_snapshots"))
	}
	var out api.WorkspaceSnapshotResult
	if reset {
		out, err = capability.ResetWorkspaceSnapshot(c.Request().Context(), req.ExpectedProof, req.Keep)
	} else {
		out, err = capability.UndoWorkspaceSnapshot(c.Request().Context(), req.ExpectedProof)
	}
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, out)
}
