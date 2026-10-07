package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	skillsdomain "github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
	skills "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

type (
	sessionCatalogDTO       = api.SessionCatalog[runtime.SessionState]
	sessionCatalogSourceDTO = api.SessionCatalogSource
	sessionResourceDTO      = api.SessionResource[runtime.SessionState]
	sessionCreateRequest    = api.SessionCreateRequest
	sessionMetadataDTO      = api.SessionMetadata
	sessionCapabilitiesDTO  = api.SessionCapabilities
	sessionThinkingLevelDTO = api.SessionThinkingLevel
	sessionInputRequest     = api.SessionInputRequest
	sessionSubmissionDTO    = api.SessionSubmission[runtime.SubmissionDisposition]
	sessionCancelRequest    = api.SessionCancelRequest
	sessionResponseRequest  = api.SessionResponseRequest[runtime.InteractionKind]
	sessionStatusDTO        = api.SessionStatus[runtime.SessionState]
	sessionPendingInputDTO  = api.SessionPendingInput
	sessionInteractionDTO   = api.SessionInteraction[runtime.InteractionKind, any]
	sessionSnapshotDTO      = api.SessionSnapshot[runtime.SessionState, runtime.InteractionKind, any]
	sessionEnvelopeDTO      = api.SessionEnvelope[any]
	sessionStreamMessage    = api.SessionStreamMessage[runtime.SessionState, runtime.InteractionKind, any]
)

const (
	sessionSourceAttribute = "docker-agent.actor.source"
	sessionAgentAttribute  = runtime.SessionAgentAttribute
)

func (s *Server) registerCanonicalSessionRoutes(group *echo.Group) {
	s.registerWorkspaceSnapshotRoutes(group)
	s.registerGeneratedMediaRoutes(group)
	group.GET("", s.sessionCatalog)
	group.POST("", s.createCanonicalSession)
	group.POST("/start", s.startCanonicalSession)
	group.GET("/:id", s.getCanonicalSession)
	group.GET("/:id/status", s.canonicalSessionStatus)
	group.GET("/:id/snapshot", s.canonicalSessionSnapshot)
	group.GET("/:id/events", s.sessionEventStream)
	group.POST("/:id/messages", s.sessionInput)
	group.POST("/:id/retry", s.retrySession)
	group.POST("/:id/responses", s.respondSession)
	group.POST("/:id/cancel", s.cancelSession)
	group.POST("/:id/stop-subtree", s.stopSessionSubtree)
	group.GET("/:id/delegation-policy", s.canonicalDelegationPolicy)
	group.PATCH("/:id/delegation-policy", s.canonicalDelegationPolicy)
	group.POST("/:id/turns/:turnID/wait", s.awaitSessionTurn)
	group.PATCH("/:id", s.editCanonicalSession)
	group.PATCH("/:id/title", s.updateCanonicalSessionTitle)
	group.GET("/:id/tree", s.sessionTree)
	group.GET("/:id/todos", s.sessionTodos)
	group.PATCH("/:id/todos/:todoID", s.canonicalEditTodo)
	group.DELETE("/:id/todos/:todoID", s.canonicalRemoveTodo)
	group.GET("/:id/tools", s.canonicalSessionTools)
	group.GET("/:id/agent-info", s.canonicalSessionAgentInfo)
	group.GET("/:id/agent-config", s.canonicalSessionAgentConfig)
	group.POST("/:id/toolsets/restart", s.canonicalRestartToolset)
	group.GET("/:id/permissions", s.canonicalSessionPermissions)
	group.GET("/:id/mcp/prompts", s.canonicalSessionPrompts)
	group.POST("/:id/mcp/prompts/execute", s.canonicalExecutePrompt)
	group.POST("/:id/branches", s.canonicalBranchSession)
	group.POST("/:id/compact", s.compactCanonicalSession)
	group.POST("/:id/compact/:target", s.compactCanonicalTarget)
	group.GET("/:id/context", s.canonicalSessionContext)
	group.GET("/:id/live-sessions", s.canonicalLiveSessions)
	group.GET("/:id/skills", s.canonicalSessionSkills)
	group.POST("/:id/skills/run", s.runCanonicalSessionSkill)
	group.GET("/:id/models", s.canonicalSessionModels)
	group.POST("/:id/models/refresh", s.refreshCanonicalSessionModels)
	group.PATCH("/:id/model", s.updateCanonicalSessionModel)
	group.GET("/:id/thinking-level", s.canonicalSessionThinkingLevel)
	group.POST("/:id/thinking-level/cycle", s.cycleCanonicalSessionThinkingLevel)
	group.PATCH("/:id/thinking-level", s.updateCanonicalSessionThinkingLevel)
	group.POST("/:id/pause", s.pauseCanonicalSession)
	group.POST("/:id/title", s.generateCanonicalSessionTitle)
	group.POST("/:id/switch-agent", s.switchCanonicalSessionAgent)
	group.PATCH("/:id/starred", s.updateCanonicalSessionStarred)
	group.DELETE("/:id/attachments", s.removeCanonicalSessionAttachment)
	group.DELETE("/:id", s.deleteCanonicalSession)
}

func decodeSessionJSON(c echo.Context, value any) error {
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain exactly one JSON value")
	}
	return nil
}

func (s *Server) sessionInput(c echo.Context) error {
	var req sessionInputRequest
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if strings.TrimSpace(req.Content) == "" && len(req.MultiContent) == 0 {
		return sessionRequestError("content or multi_content is required")
	}
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	input := runtime.TurnInput{GenerateTitle: req.GenerateTitle, Content: req.Content, MultiContent: req.MultiContent, RequestID: req.RequestID}
	var submission runtime.Submission
	switch req.Mode {
	case "", "submit":
		submission, err = handle.Submit(c.Request().Context(), input)
	case "steer":
		submission, err = handle.Steer(c.Request().Context(), input)
	default:
		return sessionRequestError("mode must be submit or steer")
	}
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusAccepted, sessionSubmissionDTO{SessionID: submission.SessionID, TurnID: submission.TurnID, Disposition: submission.Disposition})
}

func (s *Server) compactCanonicalSession(c echo.Context) error {
	return s.compactCanonical(c, c.Param("id"))
}

func (s *Server) compactCanonicalTarget(c echo.Context) error {
	return s.compactCanonical(c, c.Param("target"))
}

func (s *Server) compactCanonical(c echo.Context, target string) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	var req struct {
		AdditionalPrompt string `json:"additional_prompt,omitempty"`
	}
	if c.Request().ContentLength != 0 {
		if err := decodeSessionJSON(c, &req); err != nil {
			return sessionRequestError("invalid request body")
		}
	}
	sink := runtime.EventSinkFunc(func(runtime.Event) {})
	operationCtx := s.sm.serverCtx
	if operationCtx == nil {
		operationCtx = c.Request().Context()
	}
	if target != handle.ID() {
		capability := handle
		err = capability.CompactTarget(operationCtx, target, req.AdditionalPrompt, sink)
	} else if capability := handle; capability != nil {
		err = capability.Compact(operationCtx, req.AdditionalPrompt, sink)
	} else {
		err = &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: handle.ID(), Operation: "compact"}
	}
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusAccepted)
}

func (s *Server) canonicalSessionContext(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	result, err := capability.ContextBreakdown(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Server) canonicalLiveSessions(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	result, err := capability.LiveSessions(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Server) canonicalSessionSkills(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	available, err := capability.Skills(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	forks := slices.DeleteFunc(slices.Clone(available), func(skill skillsdomain.Skill) bool { return !skill.IsFork() })
	return c.JSON(http.StatusOK, forks)
}

func (s *Server) runCanonicalSessionSkill(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	var req struct {
		skills.RunSkillArgs

		OperationID string `json:"operation_id"`
	}
	if err := decodeSessionJSON(c, &req); err != nil || req.Name == "" {
		return sessionRequestError("skill name is required")
	}
	available, err := capability.Skills(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	if !slices.ContainsFunc(available, func(skill skillsdomain.Skill) bool { return skill.Name == req.Name && skill.IsFork() }) {
		return sessionRequestError("fork skill is not available")
	}
	operationID := req.OperationID
	if operationID == "" {
		operationID = uuid.NewString()
	}
	operationCtx := s.sm.serverCtx
	if operationCtx == nil {
		operationCtx = context.WithoutCancel(c.Request().Context())
	}
	starter := handle
	if err := starter.StartSkillFork(operationCtx, operationID, req.RunSkillArgs); err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusAccepted, map[string]string{"session_id": handle.ID(), "operation_id": operationID})
}

func (s *Server) canonicalSessionModels(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	if capability == nil || !capability.Metadata().Capabilities.ModelSwitching {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: handle.ID(), Operation: "models"})
	}
	return c.JSON(http.StatusOK, capability.AvailableModels(c.Request().Context()))
}

func (s *Server) refreshCanonicalSessionModels(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	if err := capability.RefreshModelsCatalog(c.Request().Context()); err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, sessionMetadata(handle.Metadata()))
}

func sessionThinkingLevels(ctx context.Context, handle runtime.SessionHandle) []effort.Level {
	if provider := handle; provider != nil {
		return provider.ThinkingLevels(ctx)
	}
	return slices.Clone(handle.Metadata().ThinkingLevels)
}

func sessionCurrentThinkingLevel(ctx context.Context, handle runtime.SessionHandle) effort.Level {
	if provider := handle; provider != nil {
		return provider.CurrentThinkingLevel(ctx)
	}
	return handle.Metadata().ThinkingLevel
}

func (s *Server) canonicalSessionThinkingLevel(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	if !handle.Metadata().Capabilities.ThinkingLevels {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: handle.ID(), Operation: "thinking_level"})
	}
	return c.JSON(http.StatusOK, sessionThinkingLevelDTO{Levels: sessionThinkingLevels(c.Request().Context(), handle), Current: sessionCurrentThinkingLevel(c.Request().Context(), handle), Metadata: sessionMetadata(handle.Metadata())})
}

func (s *Server) cycleCanonicalSessionThinkingLevel(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	controller := handle
	if controller == nil || !handle.Metadata().Capabilities.ThinkingLevels {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: handle.ID(), Operation: "thinking_level"})
	}
	if _, err := controller.CycleThinkingLevel(c.Request().Context()); err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, sessionThinkingLevelDTO{Levels: sessionThinkingLevels(c.Request().Context(), handle), Current: sessionCurrentThinkingLevel(c.Request().Context(), handle), Metadata: sessionMetadata(handle.Metadata())})
}

func (s *Server) updateCanonicalSessionThinkingLevel(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	var req struct {
		Level effort.Level `json:"level"`
	}
	if err := decodeSessionJSON(c, &req); err != nil || req.Level == "" {
		return sessionRequestError("level is required")
	}
	controller := handle
	if controller == nil || !handle.Metadata().Capabilities.ThinkingLevels {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: handle.ID(), Operation: "thinking_level"})
	}
	if _, err := controller.SetThinkingLevel(c.Request().Context(), req.Level); err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, sessionThinkingLevelDTO{Levels: sessionThinkingLevels(c.Request().Context(), handle), Current: sessionCurrentThinkingLevel(c.Request().Context(), handle), Metadata: sessionMetadata(handle.Metadata())})
}

func (s *Server) pauseCanonicalSession(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	paused, err := capability.TogglePause(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, map[string]bool{"paused": paused})
}

func (s *Server) switchCanonicalSessionAgent(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	var req struct {
		AgentName string `json:"agent_name"`
	}
	if err := decodeSessionJSON(c, &req); err != nil || req.AgentName == "" {
		return sessionRequestError("agent_name is required")
	}
	registry := s.sm.sessionRegistry
	if active, ok := s.sm.runtimeSessions.Load(handle.ID()); ok && active.registry != nil {
		registry = active.registry
	}
	switcher, ok := registry.(runtime.AgentSwitcher)
	if !ok {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: handle.ID(), Operation: "switch_agent"})
	}
	newSession, sess, err := switcher.SwitchAgent(c.Request().Context(), handle.ID(), req.AgentName)
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusCreated, struct {
		Metadata sessionMetadataDTO `json:"metadata"`
		Session  *session.Session   `json:"session"`
	}{sessionMetadata(newSession.Metadata()), sess})
}

func (s *Server) updateCanonicalSessionModel(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	if capability == nil || !capability.Metadata().Capabilities.ModelSwitching {
		return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: handle.ID(), Operation: "set_model"})
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if err := capability.SetModel(c.Request().Context(), req.Model); err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, sessionMetadata(handle.Metadata()))
}

func (s *Server) updateCanonicalSessionStarred(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	var req struct {
		Starred bool `json:"starred"`
	}
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if err := capability.SetStarred(c.Request().Context(), req.Starred); err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) removeCanonicalSessionAttachment(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	capability := handle
	var req struct {
		Path string `json:"path"`
	}
	if err := decodeSessionJSON(c, &req); err != nil || req.Path == "" {
		return sessionRequestError("path is required")
	}
	if err := capability.RemoveAttachment(c.Request().Context(), req.Path); err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) generateCanonicalSessionTitle(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	owner, ok := handle.(runtime.SessionTitleGenerator)
	if !ok {
		return sessionHTTPError(runtime.ErrUnsupported)
	}
	var req struct {
		Messages []string `json:"messages"`
		Replace  bool     `json:"replace"`
	}
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if err := owner.GenerateSessionTitle(c.Request().Context(), nil, req.Messages, req.Replace); err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusAccepted)
}

func (s *Server) updateCanonicalSessionTitle(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	var req struct {
		Title string `json:"title"`
	}
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if err := handle.UpdateTitle(c.Request().Context(), req.Title); err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) deleteCanonicalSession(c echo.Context) error {
	if err := s.sm.DeleteSession(c.Request().Context(), c.Param("id")); err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) retrySession(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	if c.Request().ContentLength != 0 {
		var req struct{}
		if err := decodeSessionJSON(c, &req); err != nil {
			return sessionRequestError("invalid request body")
		}
	}
	submission, err := handle.Retry(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusAccepted, sessionSubmissionDTO{SessionID: submission.SessionID, TurnID: submission.TurnID, Disposition: submission.Disposition})
}

func (s *Server) canonicalSessionStatus(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	status, err := handle.Status(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, struct {
		Metadata sessionMetadataDTO `json:"metadata"`
		Status   sessionStatusDTO   `json:"status"`
	}{Metadata: sessionMetadata(handle.Metadata()), Status: sessionStatus(status)})
}

func (s *Server) respondSession(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	var req sessionResponseRequest
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if req.InteractionID == "" || req.Kind == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "interaction_id and kind are required")
	}
	response := runtime.InteractionResponse{InteractionID: req.InteractionID, Kind: req.Kind, ClientID: req.ClientID}
	switch req.Kind {
	case runtime.InteractionConfirmation, runtime.InteractionMaxIterations:
		if req.Confirmation == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "confirmation is required")
		}
		response.Resume = runtime.ResumeRequest{Type: runtime.NormalizeResumeType(runtime.ResumeType(req.Confirmation)), Reason: req.Reason, ToolName: req.ToolName, SessionID: handle.ID(), RequestID: req.InteractionID}
	case runtime.InteractionElicitation:
		if req.Action == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "action is required")
		}
		response.ElicitationID = req.ElicitationID
		response.Elicitation = runtime.ElicitationResult{Action: tools.ElicitationAction(req.Action), Content: req.Content}
	default:
		return echo.NewHTTPError(http.StatusBadRequest, "unknown interaction kind")
	}
	if err := handle.Respond(c.Request().Context(), response); err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) cancelSession(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	var req sessionCancelRequest
	if c.Request().ContentLength != 0 {
		if err := decodeSessionJSON(c, &req); err != nil {
			return sessionRequestError("invalid request body")
		}
	}
	var result runtime.CancelResult
	if req.PendingOnly {
		canceler, ok := handle.(runtime.PendingMessageCanceler)
		if !ok {
			return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "cancel_pending_message"))
		}
		var removed bool
		removed, err = canceler.CancelPendingMessage(c.Request().Context(), req.TurnID)
		result = runtime.CancelResult{SessionID: handle.ID(), TurnID: req.TurnID, Outcome: runtime.CancelNotActive}
		if removed {
			result.Outcome = runtime.CancelAccepted
		}
	} else {
		result, err = handle.Cancel(c.Request().Context(), req.TurnID)
	}
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"session_id": result.SessionID, "turn_id": result.TurnID, "outcome": result.Outcome})
}

func (s *Server) stopSessionSubtree(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	controller, ok := handle.(runtime.SessionTreeController)
	if !ok || !handle.Metadata().Capabilities.StopSubtree {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(handle.ID(), "stop_subtree"))
	}
	if err := controller.StopSubtree(c.Request().Context()); err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) editCanonicalSession(c echo.Context) error {
	var edit runtime.SessionEdit
	if err := decodeSessionJSON(c, &edit); err != nil {
		return sessionRequestError("invalid request body")
	}
	if edit.Kind == runtime.SessionEditOpenView {
		view := c.QueryParam("view")
		if view != "" && view != "open-view" {
			return sessionRequestError("unsupported session opening view")
		}
		if view == "open-view" && edit.ViewConfirmation == nil {
			return sessionRequestError("session opening requires confirmed binding")
		}
		prepared, err := s.sm.PrepareSessionView(c.Request().Context(), c.Param("id"))
		if err != nil {
			return sessionHTTPError(err)
		}
		defer prepared.Abort()
		if edit.ViewConfirmation != nil {
			if err := edit.ViewConfirmation.Validate(prepared.Info()); err != nil {
				return sessionHTTPError(err)
			}
		}
		committed, err := prepared.Commit(c.Request().Context())
		if err != nil {
			return sessionHTTPError(err)
		}
		if view == "open-view" {
			return c.JSON(http.StatusOK, api.SessionOpened[runtime.PreparedSessionViewInfo, runtime.SessionState]{Version: api.SessionAPIVersion, View: "open-view", Info: committed.Info, Metadata: sessionMetadata(committed.Metadata), Status: sessionStatus(committed.Status)})
		}
		return c.JSON(http.StatusOK, committed.Info.Session)
	}
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	updated, err := handle.Edit(c.Request().Context(), edit)
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusOK, updated)
}

func (s *Server) awaitSessionTurn(c echo.Context) error {
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	if err := handle.AwaitTurn(c.Request().Context(), c.Param("turnID")); err != nil {
		return sessionHTTPError(err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) sessionEventStream(c echo.Context) error {
	tree := c.QueryParam("tree") == "true"
	if c.QueryParam("tree") != "" && !tree {
		return sessionRequestError("tree must be true when supplied")
	}
	handle, err := s.sessionByID(c.Request().Context(), c.Param("id"))
	if err != nil {
		return sessionHTTPError(err)
	}
	var since *uint64
	rawSince := c.QueryParam("since")
	if rawSince == "" {
		rawSince = c.Request().Header.Get("Last-Event-ID")
	}
	sinceEpoch := c.QueryParam("since_epoch")
	if sinceEpoch != "" && rawSince == "" {
		return sessionRequestError("since_epoch requires since or Last-Event-ID")
	}
	if tree && (rawSince != "" || sinceEpoch != "") {
		return sessionRequestError("tree streams do not accept since or Last-Event-ID")
	}
	if rawSince != "" {
		value, parseErr := strconv.ParseUint(rawSince, 10, 64)
		if parseErr != nil {
			return sessionRequestError("since and Last-Event-ID must be unsigned integers")
		}
		since = &value
	}
	observation, err := handle.Observe(c.Request().Context(), runtime.ObserveOptions{Since: since, SinceEpoch: sinceEpoch, Tree: tree, OrderedTree: tree})
	if err != nil {
		return sessionHTTPError(err)
	}
	defer observation.Cancel()
	response := c.Response()
	response.Header().Set(echo.HeaderContentType, "text/event-stream")
	response.Header().Set(echo.HeaderCacheControl, "no-cache")
	response.WriteHeader(http.StatusOK)
	writer := bufio.NewWriter(response)
	write := func(message sessionStreamMessage) error {
		data, err := json.Marshal(message)
		if err != nil {
			return err
		}
		if !tree && message.Envelope != nil && message.Envelope.Sequence > 0 {
			if _, err = fmt.Fprintf(writer, "id: %d\n", message.Envelope.Sequence); err != nil {
				return err
			}
		}
		if _, err = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", message.Type, data); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
		response.Flush()
		return nil
	}
	snapshots := observation.Initial
	for _, initial := range snapshots {
		snapshot := sessionSnapshot(initial)
		if err := writeSessionSnapshot(snapshot, write); err != nil {
			return nil
		}
	}
	for _, envelope := range observation.Replay {
		dto := sessionEnvelope(envelope)
		if err := write(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "event", Envelope: &dto}); err != nil {
			return nil
		}
	}
	if !tree {
		if err := write(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "ready", Cursor: observation.Primary().Cursor}); err != nil {
			return nil
		}
	} else if err := write(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "ready"}); err != nil {
		return nil
	}
	heartbeat := time.NewTicker(s.heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request().Context().Done():
			return nil
		case update, ok := <-observation.TreeUpdates:
			if !ok {
				return nil
			}
			if update.Snapshot != nil {
				dto := sessionSnapshot(*update.Snapshot)
				for _, seed := range update.Replay {
					dto.LiveSeeds = append(dto.LiveSeeds, sessionEnvelope(seed))
				}
				if err := writeSessionSnapshot(dto, write); err != nil {
					return nil
				}
			} else if update.Event != nil {
				dto := sessionEnvelope(*update.Event)
				if err := write(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "event", Envelope: &dto}); err != nil {
					return nil
				}
			}
		case snapshot, ok := <-observation.SessionsAdded:
			if !ok {
				observation.SessionsAdded = nil
				continue
			}
			dto := sessionSnapshot(snapshot)
			if err := writeSessionSnapshot(dto, write); err != nil {
				return nil
			}
		case envelope, ok := <-observation.Events:
			if !ok {
				return nil
			}
			dto := sessionEnvelope(envelope)
			if err := write(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "event", Envelope: &dto}); err != nil {
				return nil
			}
		case <-heartbeat.C:
			if _, err := writer.WriteString(": ping\n\n"); err != nil {
				return nil
			}
			if err := writer.Flush(); err != nil {
				return nil
			}
			response.Flush()
		}
	}
}

const sessionSnapshotChunkBytes = 64 << 10

// Large snapshots have bounded wire frames, not a truncated transcript.
func writeSessionSnapshot(snapshot sessionSnapshotDTO, write func(sessionStreamMessage) error) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if len(data) <= sessionSnapshotChunkBytes {
		return write(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "snapshot", Snapshot: &snapshot})
	}
	if err := write(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "snapshot_begin", Cursor: snapshot.Cursor}); err != nil {
		return err
	}
	for len(data) > 0 {
		n := min(len(data), sessionSnapshotChunkBytes)
		if err := write(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "snapshot_chunk", Cursor: snapshot.Cursor, Chunk: data[:n]}); err != nil {
			return err
		}
		data = data[n:]
	}
	return write(sessionStreamMessage{Version: api.SessionAPIVersion, Type: "snapshot_end", Cursor: snapshot.Cursor})
}

func sessionMetadata(meta runtime.SessionMetadata) sessionMetadataDTO {
	capabilities := meta.Capabilities
	return sessionMetadataDTO{SessionID: meta.SessionID, AgentName: meta.AgentName, Model: meta.Model, ThinkingLevels: meta.ThinkingLevels, ThinkingLevel: meta.ThinkingLevel, Capabilities: sessionCapabilitiesDTO{
		PendingMessageRemoval: capabilities.PendingMessageRemoval,
		Snapshots:             capabilities.Snapshots,
		ToolInspection:        capabilities.ToolInspection, ToolsetRestart: capabilities.ToolsetRestart, PermissionsInspection: capabilities.PermissionsInspection, MCPPrompts: capabilities.MCPPrompts, TodoEditing: capabilities.TodoEditing, Branching: capabilities.Branching,
		AvailableModels: capabilities.AvailableModels, Durability: string(capabilities.Durability), Compaction: capabilities.Compaction,
		TargetCompaction: capabilities.TargetCompaction, ModelSwitching: capabilities.ModelSwitching, ContextInspection: capabilities.ContextInspection,
		DelegationPolicy: capabilities.DelegationPolicy, StopSubtree: capabilities.StopSubtree, LiveSessions: capabilities.LiveSessions, SessionEditing: capabilities.SessionEditing,
		ForkSkills: capabilities.ForkSkills, Pause: capabilities.Pause, ModelCatalogRefresh: capabilities.ModelCatalogRefresh, ThinkingLevels: capabilities.ThinkingLevels, Todos: capabilities.Todos,
	}}
}

func sessionStatus(status runtime.SessionStatus) sessionStatusDTO {
	return sessionStatusDTO{
		InterruptedTurns: status.InterruptedTurns, SessionID: status.SessionID, AgentName: status.AgentName, State: status.State, Pending: status.Pending, TurnID: status.TurnID, LastError: status.LastError,
		Dormant: status.Dormant, PauseArmed: status.PauseArmed, Paused: status.Paused, PauseGeneration: status.PauseGeneration,
	}
}

func sessionSnapshot(snapshot runtime.SessionSnapshot) sessionSnapshotDTO {
	out := sessionSnapshotDTO{Session: snapshot.Session, Status: sessionStatus(snapshot.Status), Cursor: snapshot.Cursor, Epoch: snapshot.Epoch, TranscriptPosition: snapshot.TranscriptPosition, TitleStatus: snapshot.TitleStatus, Interactions: make([]sessionInteractionDTO, len(snapshot.Interactions)), PendingInputs: make([]sessionPendingInputDTO, len(snapshot.PendingInputs))}
	for _, event := range snapshot.Presentation {
		out.Presentation = append(out.Presentation, event)
	}
	if snapshot.Session != nil {
		out.ParentSessionID = snapshot.Session.ParentID
	}
	for i, interaction := range snapshot.Interactions {
		out.Interactions[i] = sessionInteractionDTO{SessionID: interaction.SessionID, InteractionID: interaction.InteractionID, Kind: interaction.Kind, ElicitationID: interaction.ElicitationID, Event: interaction.Event}
	}
	for i, input := range snapshot.PendingInputs {
		out.PendingInputs[i] = sessionPendingInputDTO{TurnID: input.TurnID, Content: input.Content, MultiContent: input.MultiContent, SessionPosition: input.SessionPosition, InputOrigin: input.InputOrigin, SenderID: input.SenderID, SenderName: input.SenderName, ReportOutcome: input.ReportOutcome, InputMode: input.InputMode}
	}
	return out
}

func sessionEnvelope(envelope runtime.SessionEvent) sessionEnvelopeDTO {
	return sessionEnvelopeDTO{Version: api.SessionAPIVersion, SessionID: envelope.SessionID, TurnID: envelope.TurnID, InteractionID: envelope.InteractionID, Sequence: envelope.Sequence, Epoch: envelope.Epoch, TranscriptPosition: envelope.TranscriptPosition, Event: sessionEventDTO(envelope.Event), Gap: envelope.Gap, FirstAvailable: envelope.FirstAvailable}
}

func sessionEventDTO(event runtime.Event) any {
	return event
}

type sessionTransportError struct {
	Status    int
	Code      string
	Operation string
	SessionID string
	Message   string
}

func (e *sessionTransportError) Error() string { return e.Message }

func nonAttachableSessionError(id string) error {
	return &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: id, Operation: "attach"}
}

func sessionRequestError(message string) error {
	return echo.NewHTTPError(http.StatusBadRequest, map[string]any{"error": "invalid_request", "operation": "request", "message": message})
}

func publicSessionOperation(operation string) string {
	switch operation {
	case "restore_binding", "restore_parent", "restore_tree_membership", "restore_ancestry", "restore_source", "restore_child_tree":
		return "attach"
	case "create_actor": // durable compatibility operation from older runtimes
		return "create_session"
	default:
		return operation
	}
}

func sessionHTTPError(err error) error {
	if transportErr, ok := errors.AsType[*sessionTransportError](err); ok {
		return echo.NewHTTPError(transportErr.Status, map[string]any{"error": transportErr.Code, "operation": transportErr.Operation, "session_id": transportErr.SessionID, "message": transportErr.Message})
	}
	if sessionErr, ok := errors.AsType[*runtime.SessionError](err); ok {
		status := http.StatusConflict
		switch sessionErr.Kind {
		case runtime.SessionErrorInvalid:
			status = http.StatusBadRequest
		case runtime.SessionErrorNotFound:
			status = http.StatusNotFound
		case runtime.SessionErrorCapacity:
			status = http.StatusTooManyRequests
		case runtime.SessionErrorPersistence:
			status = http.StatusServiceUnavailable
		case runtime.SessionErrorStale:
			status = http.StatusPreconditionFailed
		case runtime.SessionErrorUnsupported:
			status = http.StatusNotImplemented
		case runtime.SessionErrorConflict, runtime.SessionErrorWrongSession, runtime.SessionErrorInterrupted:
			status = http.StatusConflict
		}
		payload := map[string]any{"error": sessionErr.Kind, "operation": publicSessionOperation(string(sessionErr.Operation)), "reason": sessionErr.Reason, "session_id": sessionErr.SessionID}
		if sessionErr.Detail != "" {
			payload["detail"] = sessionErr.Detail
		}
		return echo.NewHTTPError(status, payload)
	}
	if errors.Is(err, session.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "session not found")
	}
	if errors.Is(err, ErrInvalidWorkingDir) {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
}
