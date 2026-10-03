package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

const sessionStartProofAttribute = "docker-agent.session.start-proof"

func (s *Server) startCanonicalSession(c echo.Context) error {
	var req api.SessionStartRequest
	if err := decodeSessionJSON(c, &req); err != nil {
		return sessionRequestError("invalid request body")
	}
	if !validSessionCreateID(req.SessionID) || req.ParentSessionID != "" || strings.TrimSpace(req.AgentName) == "" || req.Input.RequestID == "" || (req.Input.Mode != "" && req.Input.Mode != "submit") || (strings.TrimSpace(req.Input.Content) == "" && len(req.Input.MultiContent) == 0) {
		return sessionRequestError("session_id, agent_name and input.request_id are required for a root submit")
	}
	ctx := c.Request().Context()
	registry, source, err := s.sm.sessionRegistryForCreate(req.Source)
	if err != nil {
		return sessionHTTPError(err)
	}
	req.Source, req.Input.Mode = source, "submit"
	// Hash only the supplied creation contract, not mutable live bindings or defaults.
	encoded, err := json.Marshal(req)
	if err != nil {
		return sessionHTTPError(err)
	}
	digest := sha256.Sum256(encoded)
	proof := hex.EncodeToString(digest[:])
	unlock := s.sm.sessionRestoreLocks.lock(req.SessionID)
	var handle runtime.SessionHandle
	stored, err := s.sm.sessionStore.GetSession(ctx, req.SessionID)
	switch {
	case err == nil:
		if stored.AttributesSnapshot()[sessionStartProofAttribute] != proof {
			unlock()
			return sessionHTTPError(&runtime.SessionError{Kind: runtime.SessionErrorConflict, SessionID: req.SessionID, Operation: "start_session"})
		}
		unlock()
		handle, err = s.sm.Handle(ctx, req.SessionID)
	case errors.Is(err, session.ErrNotFound):
		template := &session.Session{Title: req.Title, WorkingDir: req.WorkingDir, SafetyPolicy: req.SafetyPolicy, ToolsApproved: req.ToolsApproved, Permissions: req.Permissions}
		template.SetAttribute(sessionAgentAttribute, req.AgentName)
		template.SetAttribute(sessionSourceAttribute, source)
		template.SetAttribute(sessionStartProofAttribute, proof)
		var sess *session.Session
		sess, err = s.sm.prepareSession(template)
		if err == nil {
			sess.ID, sess.AgentName = req.SessionID, req.AgentName
			if sess.GetSafetyPolicy() == "" && !sess.ToolsApproved {
				if defaults, ok := registry.(runtime.SafetyDefaults); ok {
					sess.SetSafetyPolicy(authorSafetyDefault(ctx, defaults, sess))
				}
			}
			handle, err = s.sm.createHTTPSession(ctx, registry, sess, runtime.SessionBinding{AgentName: req.AgentName, Model: req.Model})
		}
		unlock()
	default:
		unlock()
	}
	if err != nil {
		return sessionHTTPError(err)
	}
	submission, err := handle.Submit(ctx, runtime.TurnInput{Content: req.Input.Content, MultiContent: req.Input.MultiContent, RequestID: req.Input.RequestID})
	if err != nil {
		return sessionHTTPError(err)
	}
	return c.JSON(http.StatusAccepted, sessionSubmissionDTO{SessionID: submission.SessionID, TurnID: submission.TurnID, Disposition: submission.Disposition})
}
