package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// The old HTTP surface is a request/response projection over the same manager
// and session handles as v2. There is deliberately no legacy registry or loop.
func (s *Server) registerLegacySessionRoutes(g *echo.Group) {
	g.GET("", s.legacyListSessions)
	g.POST("", s.legacyCreateSession)
	g.GET("/:id", s.legacyGetSession)
	g.GET("/:id/status", s.legacySessionStatus)
	g.GET("/:id/snapshot", s.legacySessionSnapshot)
	g.POST("/:id/resume", s.legacyResumeSession)
	g.POST("/:id/elicitation", s.legacyElicitation)
	g.POST("/:id/agent/:agent", s.legacyRunAgent)
	g.POST("/:id/agent/:agent/:agent_name", s.legacyRunAgent)
	g.POST("/:id/steer", s.legacySteerSession)
	g.POST("/:id/followup", s.legacyFollowUpSession)
	g.GET("/:id/queue", s.legacyQueueStatus)
	g.GET("/:id/events", s.legacySessionEvents)
	g.POST("/:id/messages", s.legacyAddMessage)
	g.PATCH("/:id/title", s.legacyUpdateTitle)
	g.PATCH("/:id/starred", s.legacySetStarred)
	g.GET("/:id/models", s.legacySessionModels)
	g.DELETE("/:id", s.legacyDeleteSession)
	// Existing metadata adapters already route bound rows through handle.Edit.
	g.POST("/:id/tools/toggle", s.toggleSessionYolo)
	g.PATCH("/:id/safety-policy", s.updateSessionSafetyPolicy)
	g.PATCH("/:id/permissions", s.updateSessionPermissions)
	g.PATCH("/:id/tokens", s.updateSessionTokens)
	g.POST("/:id/fork", s.forkSession)
	g.PATCH("/:id/messages/:msg_id", s.updateMessage)
	g.POST("/:id/summaries", s.addSummary)
	g.GET("/:id/recovery", s.getSessionRecoveryData)
	g.POST("/batch/delete", s.batchDeleteSessions)
	g.POST("/batch/export", s.batchExportSessions)
}

func legacyStreaming(status runtime.SessionStatus) bool {
	return status.State == runtime.SessionStateRunning || status.State == runtime.SessionStateCancelling
}

func legacyDetail(sess *session.Session) api.SessionResponse {
	input, output := sess.Usage()
	return api.SessionResponse{ID: sess.ID, Title: sess.TitleSnapshot(), CreatedAt: sess.CreatedAt, Messages: sess.GetAllMessages(), ToolsApproved: sess.ToolsApproved, SafetyPolicy: sess.SafetyPolicy, InputTokens: input, OutputTokens: output, WorkingDir: sess.WorkingDir, Permissions: sess.ClonePermissions()}
}

func (s *Server) legacyCreateSession(c echo.Context) error {
	var template session.Session
	if err := c.Bind(&template); err != nil {
		return sessionRequestError("invalid request body")
	}
	sess, err := s.sm.CreateSession(c.Request().Context(), &template)
	if err != nil {
		if errors.Is(err, ErrInvalidWorkingDir) {
			return echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, sess)
}

func (s *Server) legacyGetSession(c echo.Context) error {
	sess, err := s.sm.GetSession(c.Request().Context(), c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "session not found")
	}
	return c.JSON(http.StatusOK, legacyDetail(sess))
}

func (s *Server) legacyListSessions(c echo.Context) error {
	ctx := c.Request().Context()
	out := []api.SessionsResponse{}
	appendRow := func(sess *session.Session, streaming, active bool) {
		input, output := sess.Usage()
		count := len(sess.GetAllMessages())
		if active {
			count = 0
		}
		out = append(out, api.SessionsResponse{ID: sess.ID, Title: sess.TitleSnapshot(), CreatedAt: sess.CreatedAt.Format(time.RFC3339), NumMessages: count, InputTokens: input, OutputTokens: output, WorkingDir: sess.WorkingDir, Streaming: streaming})
	}
	if c.QueryParam("active") == "true" {
		var failure error
		s.sm.runtimeSessions.Range(func(_ string, rs *activeRuntimes) bool {
			if rs.handle == nil {
				return true
			}
			snapshot, err := rs.handle.Snapshot(ctx)
			if err != nil {
				failure = err
				return false
			}
			status, err := rs.handle.Status(ctx)
			if err != nil {
				failure = err
				return false
			}
			appendRow(snapshot, legacyStreaming(status), true)
			return true
		})
		if failure != nil {
			return sessionHTTPError(failure)
		}
	} else {
		rows, err := s.sm.GetSessions(ctx)
		if err != nil {
			return sessionHTTPError(err)
		}
		for _, row := range rows {
			appendRow(row, false, false)
		}
	}
	return c.JSON(http.StatusOK, out)
}

func (s *Server) legacyAttached(id string) runtime.SessionHandle {
	if h := s.sm.loadedSession(id); h != nil {
		return h
	}
	// Borrowed --listen registries can already own a session without the HTTP
	// manager having served it. Lookup is read-only and does not restore.
	if s.sm.sessionRegistry != nil {
		if h, err := s.sm.sessionRegistry.SessionByID(id); err == nil {
			return h
		}
	}
	return nil
}

func (s *Server) legacySessionStatus(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	h := s.legacyAttached(id)
	if raw := c.QueryParam("wait"); raw != "" {
		wait, err := time.ParseDuration(raw)
		if err != nil {
			return sessionRequestError("invalid wait")
		}
		waitCtx, cancel := context.WithTimeout(ctx, min(wait, maxAPITimeout))
		defer cancel()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for h == nil {
			select {
			case <-waitCtx.Done():
				return echo.NewHTTPError(http.StatusServiceUnavailable, "session not ready within timeout")
			case <-ticker.C:
				h = s.legacyAttached(id)
			}
		}
	}
	if h == nil {
		return echo.NewHTTPError(http.StatusNotFound, "session not found")
	}
	snapshot, err := h.Snapshot(ctx)
	if err != nil {
		return sessionHTTPError(err)
	}
	status, err := h.Status(ctx)
	if err != nil {
		return sessionHTTPError(err)
	}
	input, output := snapshot.Usage()
	return c.JSON(http.StatusOK, api.SessionStatusResponse{ID: id, Title: snapshot.TitleSnapshot(), Streaming: legacyStreaming(status), Agent: h.AgentName(), InputTokens: input, OutputTokens: output, NumMessages: len(snapshot.GetAllMessages())})
}

func (s *Server) legacySessionSnapshot(c echo.Context) error {
	ctx := c.Request().Context()
	id := c.Param("id")
	var snapshot *session.Session
	status := runtime.SessionStatus{}
	var cursor uint64
	if h := s.legacyAttached(id); h != nil {
		observation, err := h.Observe(ctx, runtime.ObserveOptions{})
		if err != nil {
			return sessionHTTPError(err)
		}
		initial := observation.Primary()
		observation.Cancel()
		snapshot, status, cursor = initial.Session, initial.Status, initial.Cursor
	} else {
		var err error
		snapshot, err = s.sm.GetSession(ctx, id)
		if err != nil {
			return c.JSON(http.StatusNotFound, api.ErrorResponse{Code: api.ErrCodeUnknownSession, Message: "session not found"})
		}
	}
	if snapshot == nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "session snapshot unavailable")
	}
	input, output, cost := snapshot.TokensAndCost()
	return c.JSON(http.StatusOK, api.SessionSnapshotResponse{ID: id, Title: snapshot.TitleSnapshot(), CreatedAt: snapshot.CreatedAt, WorkingDir: snapshot.WorkingDir, Messages: snapshot.GetAllMessages(), ToolsApproved: snapshot.ToolsApproved, SafetyPolicy: snapshot.SafetyPolicy, Permissions: snapshot.ClonePermissions(), InputTokens: input, OutputTokens: output, Cost: cost, Streaming: legacyStreaming(status), Agent: status.AgentName, LastEventSeq: cursor})
}

// Legacy responses carry no canonical interaction ID. Resolve only an exact
// single matching outstanding prompt; ambiguity is never guessed or cached.
func (s *Server) legacyInteraction(c echo.Context, elicitation bool, elicitationID string) (runtime.SessionHandle, runtime.InteractionSnapshot, error) {
	h := s.legacyAttached(c.Param("id"))
	if h == nil {
		return nil, runtime.InteractionSnapshot{}, echo.NewHTTPError(http.StatusNotFound, "session not found")
	}
	observation, err := h.Observe(c.Request().Context(), runtime.ObserveOptions{})
	if err != nil {
		return nil, runtime.InteractionSnapshot{}, sessionHTTPError(err)
	}
	defer observation.Cancel()
	var match runtime.InteractionSnapshot
	count := 0
	for _, prompt := range observation.Primary().Interactions {
		if (prompt.Kind == runtime.InteractionElicitation) != elicitation {
			continue
		}
		if elicitationID != "" && prompt.ElicitationID != elicitationID {
			continue
		}
		match = prompt
		count++
	}
	if count != 1 {
		return nil, runtime.InteractionSnapshot{}, echo.NewHTTPError(http.StatusConflict, "legacy response requires exactly one matching pending interaction")
	}
	return h, match, nil
}

func (s *Server) legacyResumeSession(c echo.Context) error {
	var req api.ResumeSessionRequest
	if err := c.Bind(&req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if req.Confirmation == "" {
		return sessionRequestError("confirmation is required")
	}
	h, prompt, err := s.legacyInteraction(c, false, "")
	if err != nil {
		return err
	}
	err = h.Respond(c.Request().Context(), runtime.InteractionResponse{InteractionID: prompt.InteractionID, Kind: prompt.Kind, Resume: runtime.ResumeRequest{Type: runtime.NormalizeResumeType(runtime.ResumeType(req.Confirmation)), Reason: req.Reason, ToolName: req.ToolName, SessionID: h.ID(), RequestID: prompt.InteractionID}})
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "session resumed"})
}

func (s *Server) legacyElicitation(c echo.Context) error {
	var req api.ResumeElicitationRequest
	if err := c.Bind(&req); err != nil {
		return sessionRequestError("invalid request body")
	}
	h, prompt, err := s.legacyInteraction(c, true, req.ElicitationID)
	if err != nil {
		return err
	}
	err = h.Respond(c.Request().Context(), runtime.InteractionResponse{InteractionID: prompt.InteractionID, Kind: prompt.Kind, ElicitationID: prompt.ElicitationID, Elicitation: runtime.ElicitationResult{Action: tools.ElicitationAction(req.Action), Content: req.Content}})
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, nil)
}

func (s *Server) legacyUpdateTitle(c echo.Context) error {
	var req api.UpdateSessionTitleRequest
	if err := c.Bind(&req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if err := s.sm.UpdateSessionTitle(c.Request().Context(), c.Param("id"), req.Title); err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, api.UpdateSessionTitleResponse{ID: c.Param("id"), Title: req.Title})
}

func (s *Server) legacySetStarred(c echo.Context) error {
	var req api.SetSessionStarredRequest
	if err := c.Bind(&req); err != nil {
		return sessionRequestError("invalid request body")
	}
	ctx := c.Request().Context()
	id := c.Param("id")
	sess, err := s.sm.GetSession(ctx, id)
	if err != nil {
		return sessionHTTPError(err)
	}
	if sess.ParentID != "" || sess.AttributesSnapshot()[sessionAgentAttribute] != "" || s.legacyAttached(id) != nil {
		h, err := s.sm.Handle(ctx, id)
		if err != nil {
			return sessionHTTPError(err)
		}
		if err := h.SetStarred(ctx, req.Starred); err != nil {
			return sessionHTTPError(err)
		}
	} else {
		if err := s.sm.sessionStore.SetSessionStarred(ctx, id, req.Starred); err != nil {
			return sessionHTTPError(err)
		}
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

func (s *Server) legacyAddMessage(c echo.Context) error {
	var req api.AddMessageRequest
	if err := c.Bind(&req); err != nil || req.Message == nil {
		return sessionRequestError("message is required")
	}
	err := s.sm.editSession(c.Request().Context(), c.Param("id"), runtime.SessionEdit{Kind: runtime.SessionEditMessage, Message: req.Message, MessageIndex: -1})
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusCreated, map[string]string{"status": "added"})
}

func (s *Server) legacySessionModels(c echo.Context) error {
	h := s.legacyAttached(c.Param("id"))
	if h == nil {
		return echo.NewHTTPError(http.StatusNotFound, "session not found or not running")
	}
	if !h.Metadata().Capabilities.ModelSwitching {
		return echo.NewHTTPError(http.StatusUnprocessableEntity, "model switching is not supported")
	}
	return c.JSON(http.StatusOK, runtime.SessionModelsResponse{Agent: h.AgentName(), CurrentModelRef: h.Metadata().Model, Models: h.AvailableModels(c.Request().Context())})
}

func (s *Server) legacyDeleteSession(c echo.Context) error {
	timeout := 10 * time.Second
	if raw := c.QueryParam("timeout"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return sessionRequestError("invalid timeout")
		}
		timeout = min(parsed, maxAPITimeout)
	}
	ctx := c.Request().Context()
	if c.QueryParam("wait") == "true" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if err := s.sm.DeleteSession(ctx, c.Param("id")); err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, map[string]string{"message": "session deleted"})
}
