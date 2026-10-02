package api

import (
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

// SessionToolsetStatus is safe for transport: errors never expose credentials.
type SessionToolsetStatus struct {
	Name         string          `json:"name"`
	Kind         string          `json:"kind,omitempty"`
	Description  string          `json:"description,omitempty"`
	State        lifecycle.State `json:"state"`
	LastError    string          `json:"last_error,omitempty"`
	RestartCount int             `json:"restart_count"`
	Restartable  bool            `json:"restartable"`
}
type SessionToolsInfo struct {
	Tools    []tools.Tool           `json:"tools"`
	Statuses []SessionToolsetStatus `json:"statuses"`
}
type SessionTodoPatch struct {
	Status              *string `json:"status,omitempty"`
	Description         *string `json:"description,omitempty"`
	ExpectedDescription *string `json:"expected_description,omitempty"`
}
type SessionPromptRequest struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args,omitempty"`
}
type SessionPromptResult struct {
	Text string `json:"text"`
}
type SessionBranchRequest struct {
	Position         *int   `json:"position,omitempty"`
	ExpectedSnapshot string `json:"expected_snapshot,omitempty"`
}
type SessionBranchResult struct {
	Metadata SessionMetadata  `json:"metadata"`
	Session  *session.Session `json:"session"`
}
