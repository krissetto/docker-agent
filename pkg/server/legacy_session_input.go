package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
)

func legacyInputs(messages []api.Message) []runtime.TurnInput {
	inputs := make([]runtime.TurnInput, 0, len(messages))
	// Upstream ignored role and recorded every supplied message as user input.
	for _, message := range messages {
		inputs = append(inputs, runtime.TurnInput{Content: message.Content, MultiContent: message.MultiContent})
	}
	return inputs
}

func legacyInputHTTPError(c echo.Context, err error, queue string) error {
	var typed *runtime.SessionError
	if errors.As(err, &typed) {
		if typed.Kind == runtime.SessionErrorCapacity {
			if typed.Reason == runtime.SessionErrorReasonBusy {
				return echo.NewHTTPError(http.StatusConflict, err.Error())
			}
			c.Response().Header().Set("Retry-After", "1")
			return echo.NewHTTPError(http.StatusTooManyRequests, queue+" queue full")
		}
	}
	return sessionHTTPError(err)
}

func (s *Server) legacyRunAgent(c echo.Context) error {
	var req api.RunAgentRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&req); err != nil {
		return sessionRequestError("invalid request body")
	}
	h, err := s.sm.resolveLegacyRun(c.Request().Context(), c.Param("id"), c.Param("agent"), c.Param("agent_name"))
	if err != nil {
		return agentSourceHTTPError("failed to run session", err)
	}
	commands, hasCommands := h.(runtime.SessionLegacyCommandInput)
	if !hasCommands {
		if err := s.sm.rejectUnsupportedLegacyCommands(c.Request().Context(), h, c.Param("agent"), req.Messages); err != nil {
			return sessionHTTPError(err)
		}
	}
	input, ok := h.(runtime.SessionLegacyInput)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(h.ID(), "legacy_run"))
	}
	status, err := h.Status(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	if legacyStreaming(status) {
		return echo.NewHTTPError(http.StatusConflict, "session is already processing a request")
	}
	if req.Model != "" {
		if !h.Metadata().Capabilities.ModelSwitching {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "model switching is not supported")
		}
		if _, ok := h.(runtime.SessionLegacyModelInput); !ok && !hasCommands {
			return echo.NewHTTPError(http.StatusUnprocessableEntity, "atomic legacy model admission is not supported")
		}
	}

	observation, err := h.Observe(c.Request().Context(), runtime.ObserveOptions{})
	if err != nil {
		return sessionHTTPError(err)
	}
	var submission runtime.Submission
	switch {
	case hasCommands:
		messages := make([]runtime.LegacyCommandMessage, len(req.Messages))
		for i, message := range req.Messages {
			messages[i] = runtime.LegacyCommandMessage{Role: message.Role, Input: runtime.TurnInput{Content: message.Content, MultiContent: message.MultiContent}}
		}
		submission, err = commands.RunLegacyCommandTurn(c.Request().Context(), messages, req.Model, "")
	case req.Model != "":
		submission, err = h.(runtime.SessionLegacyModelInput).RunLegacyTurnWithModel(c.Request().Context(), legacyInputs(req.Messages), req.Model, "")
	default:
		submission, err = input.RunLegacyTurn(c.Request().Context(), legacyInputs(req.Messages), "")
	}
	if err != nil {
		if observation.Cancel != nil {
			observation.Cancel()
		}
		return legacyInputHTTPError(c, err, "run")
	}
	return s.legacyRunStream(c, h, observation, submission.TurnID, true)
}

func (s *Server) legacySteerSession(c echo.Context) error {
	var req api.SteerSessionRequest
	if err := c.Bind(&req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if len(req.Messages) == 0 {
		return sessionRequestError("at least one message is required")
	}
	h := s.legacyAttached(c.Param("id"))
	if h == nil {
		return echo.NewHTTPError(http.StatusConflict, "session is not running")
	}
	for _, input := range legacyInputs(req.Messages) {
		if _, err := h.Steer(c.Request().Context(), input); err != nil {
			return legacyInputHTTPError(c, err, "steer")
		}
	}
	return c.JSON(http.StatusAccepted, map[string]string{"status": "queued"})
}

func (s *Server) legacyFollowUpSession(c echo.Context) error {
	var req api.SteerSessionRequest
	if err := c.Bind(&req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if len(req.Messages) == 0 {
		return sessionRequestError("at least one message is required")
	}
	h := s.legacyAttached(c.Param("id"))
	if h == nil {
		return echo.NewHTTPError(http.StatusConflict, "session is not running")
	}
	input, ok := h.(runtime.SessionLegacyInput)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(h.ID(), "legacy_followup"))
	}
	result, err := input.QueueLegacyFollowUps(c.Request().Context(), legacyInputs(req.Messages), c.Request().Header.Get("Idempotency-Key"))
	if err != nil {
		return legacyInputHTTPError(c, err, "follow-up")
	}
	status := "queued_idle"
	if result.Streaming {
		status = "queued_streaming"
	}
	if result.Duplicate {
		status = "duplicate"
	}
	return c.JSON(http.StatusAccepted, api.FollowUpResponse{Status: status, Duplicate: result.Duplicate})
}

func (s *Server) legacyQueueStatus(c echo.Context) error {
	h := s.legacyAttached(c.Param("id"))
	if h == nil {
		return echo.NewHTTPError(http.StatusNotFound, "session not found or not running")
	}
	input, ok := h.(runtime.SessionLegacyInput)
	if !ok {
		return sessionHTTPError(runtime.UnsupportedSessionOperation(h.ID(), "legacy_queue"))
	}
	status, err := input.LegacyQueueStatus(c.Request().Context())
	if err != nil {
		return sessionHTTPError(err)
	}
	response := api.QueueDepthResponse{}
	response.Steer.Depth, response.Steer.Capacity = status.SteerDepth, status.SteerCapacity
	response.Followup.Depth, response.Followup.Capacity = status.FollowUpDepth, status.FollowUpCapacity
	return c.JSON(http.StatusOK, response)
}
