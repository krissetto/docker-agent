package api

import "github.com/docker/docker-agent/pkg/teamdef"

// SessionAgentConfig contains inspector metadata. Definition, when present for
// a historical view, is a credential-safe authored projection; instructions
// are authored text and may contain literal secrets. Resolved environment and
// provider credentials are never included.
type SessionAgentConfig[Info any] struct {
	SessionID          string                   `json:"session_id"`
	Source             string                   `json:"source,omitempty"`
	AgentName          string                   `json:"agent_name"`
	Info               Info                     `json:"info"`
	Historical         bool                     `json:"historical,omitempty"`
	ExecutionAvailable bool                     `json:"execution_available,omitempty"`
	HistoryReason      string                   `json:"history_reason,omitempty"`
	AgentID            string                   `json:"agent_id,omitempty"`
	AgentKey           string                   `json:"agent_key,omitempty"`
	DefinitionKnown    bool                     `json:"definition_known,omitempty"`
	Definition         *teamdef.AgentDefinition `json:"definition,omitempty"`
}
