package api

import "github.com/docker/docker-agent/pkg/session"

// ResumeSessionRequest is the deployed unversioned confirmation request.
// Adapters translate these request shapes into canonical session operations;
// they own no runtime state.
type ResumeSessionRequest struct {
	Confirmation string `json:"confirmation"`
	Reason       string `json:"reason,omitempty"`
	ToolName     string `json:"tool_name,omitempty"`
}
type ResumeElicitationRequest struct {
	Action        string         `json:"action"`
	Content       map[string]any `json:"content"`
	ElicitationID string         `json:"elicitation_id,omitempty"`
}
type SteerSessionRequest struct {
	Messages []Message `json:"messages"`
}
type RunAgentRequest struct {
	Messages []Message `json:"messages"`
	Model    string    `json:"model,omitempty"`
}
type FollowUpResponse struct {
	Status    string `json:"status"`
	Duplicate bool   `json:"duplicate"`
}
type AddMessageRequest struct {
	Message *session.Message `json:"message"`
}
