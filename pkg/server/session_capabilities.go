package server

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func (s *Server) canonicalSessionTools(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability, ok := handle.(runtime.SessionToolInspector)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "inspect_tools"))
	}
	out, err := capability.InspectTools(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, out)
}
func (s *Server) canonicalRestartToolset(c echo.Context) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeSessionJSON(c, &req); err != nil || req.Name == "" {
		return sessionRequestError("name is required")
	}
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability, ok := handle.(runtime.SessionToolsetController)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "restart_toolset"))
	}
	if err := capability.RestartToolset(c.Request().Context(), req.Name); err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}
func (s *Server) canonicalSessionPermissions(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability, ok := handle.(runtime.SessionPermissionsInspector)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "permissions"))
	}
	out, err := capability.EffectivePermissions(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, out)
}
func (s *Server) canonicalSessionPrompts(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability, ok := handle.(runtime.SessionMCPPrompts)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "mcp_prompts"))
	}
	out, err := capability.MCPPrompts(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, out)
}
func (s *Server) canonicalExecutePrompt(c echo.Context) error {
	var req api.SessionPromptRequest
	if err := decodeSessionJSON(c, &req); err != nil || req.Name == "" {
		return sessionRequestError("name is required")
	}
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability, ok := handle.(runtime.SessionMCPPrompts)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "mcp_prompt"))
	}
	text, err := capability.ExecuteMCPPrompt(c.Request().Context(), req.Name, req.Args)
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, api.SessionPromptResult{Text: text})
}
func (s *Server) canonicalEditTodo(c echo.Context) error {
	var req api.SessionTodoPatch
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if (req.Status == nil) == (req.Description == nil) {
		return sessionRequestError("exactly one of status or description is required")
	}
	if req.Description != nil && req.ExpectedDescription == nil {
		return sessionRequestError("expected_description is required")
	}
	if req.Status != nil && req.ExpectedDescription != nil {
		return sessionRequestError("expected_description requires description")
	}
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	var out []session.Todo
	if req.Status != nil {
		out, err = handle.SetTodoStatus(c.Request().Context(), c.Param("todoID"), *req.Status)
	} else {
		out, err = handle.SetTodoDescription(c.Request().Context(), c.Param("todoID"), *req.ExpectedDescription, *req.Description)
	}
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, out)
}
func (s *Server) canonicalRemoveTodo(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	out, err := handle.RemoveTodo(c.Request().Context(), c.Param("todoID"))
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, out)
}
func (s *Server) canonicalBranchSession(c echo.Context) error {
	var req api.SessionBranchRequest
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	handle, sess, err := s.sm.BranchSession(c.Request().Context(), c.Param("id"), req)
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusCreated, api.SessionBranchResult{Metadata: sessionMetadata(handle.Metadata()), Session: sess})
}
func (sm *SessionManager) BranchSession(ctx context.Context, id string, options runtime.BranchOptions) (runtime.SessionHandle, *session.Session, error) {
	handle, err := sm.Handle(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	registry := sm.sessionRegistry
	if active, ok := sm.runtimeSessions.Load(handle.ID()); ok && active.registry != nil {
		registry = active.registry
	}
	brancher, ok := registry.(runtime.SessionBrancher)
	if !ok {
		return nil, nil, runtime.UnsupportedSessionOperation(id, "branch")
	}
	return brancher.BranchSession(ctx, id, options)
}
func (w *workspaceSessionRuntimes) BranchSession(ctx context.Context, id string, options runtime.BranchOptions) (runtime.SessionHandle, *session.Session, error) {
	rt, key, ok := w.owner(id)
	if !ok {
		return nil, nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "branch"}
	}
	brancher, ok := rt.(runtime.SessionBrancher)
	if !ok {
		return nil, nil, runtime.UnsupportedSessionOperation(id, "branch")
	}
	handle, sess, err := brancher.BranchSession(ctx, id, options)
	if err == nil {
		w.remember(handle.ID(), key)
	}
	return handle, sess, err
}

func (s *Server) canonicalSessionAgentInfo(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability, ok := handle.(runtime.SessionAgentInfoProvider)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "agent_info"))
	}
	info, err := capability.SessionAgentInfo(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, info)
}
