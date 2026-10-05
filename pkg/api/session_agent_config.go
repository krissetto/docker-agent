package api

// SessionAgentConfig contains only the inspector's display metadata, never the
// source configuration, instructions, environment, or tool connection settings.
type SessionAgentConfig[Info any] struct {
	SessionID string `json:"session_id"`
	Source    string `json:"source,omitempty"`
	AgentName string `json:"agent_name"`
	Info      Info   `json:"info"`
}
