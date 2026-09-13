package session

import "encoding/json"

// Execution settings exclude live toolsets and runtime synchronization state.
type executionSettings struct {
	AgentName               string       `json:"agent_name,omitempty"`
	AsyncSubagent           bool         `json:"async_subagent,omitempty"`
	AttachedFiles           []string     `json:"attached_files,omitempty"`
	DelegationLineage       []string     `json:"delegation_lineage,omitempty"`
	NonInteractive          bool         `json:"non_interactive,omitempty"`
	HideToolResults         bool         `json:"hide_tool_results,omitempty"`
	PriorSafetyPolicy       SafetyPolicy `json:"prior_safety_policy,omitempty"`
	MaxConsecutiveToolCalls int          `json:"max_consecutive_tool_calls,omitempty"`
	MaxOldToolCallTokens    int          `json:"max_old_tool_call_tokens,omitempty"`
	MaxToolResultTokens     int          `json:"max_tool_result_tokens,omitempty"`
}

func encodeExecutionSettings(s *Session) (string, error) {
	settings := executionSettings{
		AgentName: s.AgentName, AsyncSubagent: s.AsyncSubagent,
		AttachedFiles: s.AttachedFiles, DelegationLineage: s.DelegationLineage,
		NonInteractive: s.NonInteractive, HideToolResults: s.HideToolResults,
		PriorSafetyPolicy:       s.PriorSafetyPolicy,
		MaxConsecutiveToolCalls: s.MaxConsecutiveToolCalls,
		MaxOldToolCallTokens:    s.MaxOldToolCallTokens, MaxToolResultTokens: s.MaxToolResultTokens,
	}
	data, err := json.Marshal(settings)
	return string(data), err
}

func decodeExecutionSettings(s *Session, data string) error {
	if data == "" {
		return nil
	}
	var settings executionSettings
	if err := json.Unmarshal([]byte(data), &settings); err != nil {
		return err
	}
	s.AgentName, s.AsyncSubagent = settings.AgentName, settings.AsyncSubagent
	s.AttachedFiles, s.DelegationLineage = settings.AttachedFiles, settings.DelegationLineage
	s.NonInteractive, s.HideToolResults = settings.NonInteractive, settings.HideToolResults
	s.PriorSafetyPolicy = settings.PriorSafetyPolicy
	s.MaxConsecutiveToolCalls = settings.MaxConsecutiveToolCalls
	s.MaxOldToolCallTokens, s.MaxToolResultTokens = settings.MaxOldToolCallTokens, settings.MaxToolResultTokens
	return nil
}
