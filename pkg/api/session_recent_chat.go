package api

const (
	RecentChatDefaultLimit     = 12
	RecentChatMaxLimit         = 24
	RecentChatMaxResponseBytes = 32 * 1024
	RecentChatMaxItemTextBytes = 2048
)

// RecentChat is a bounded, read-only chat projection. Cursors are opaque to
// clients and bind a stable session ID to the item position preceding a page.
// RuntimeStatus is historical, offline, resident, read_only, or unknown; it
// never implies that a cold session can execute.
// Pending counts queued input; attention counts are explicit and known only
// when read from a resident runtime owner.
type RecentChat struct {
	Version            int                 `json:"version"`
	SessionID          string              `json:"session_id"`
	RootSessionID      string              `json:"root_session_id"`
	Source             string              `json:"source,omitempty"`
	AgentID            string              `json:"agent_id,omitempty"`
	AgentName          string              `json:"agent_name,omitempty"`
	RuntimeStatus      string              `json:"runtime_status"`
	State              string              `json:"state,omitempty"`
	ExecutionAvailable bool                `json:"execution_available"`
	Pending            int                 `json:"pending"`
	AttentionKnown     bool                `json:"attention_known"`
	ApprovalCount      int                 `json:"approval_count"`
	ElicitationCount   int                 `json:"elicitation_count"`
	Messages           []RecentChatMessage `json:"messages"`
	TruncatedBefore    bool                `json:"truncated_before"`
	BeforeCursor       string              `json:"before_cursor,omitempty"`
}

type RecentChatMessage struct {
	ID        int64  `json:"id"`
	Position  int    `json:"position"`
	Role      string `json:"role"`
	AgentName string `json:"agent_name,omitempty"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}
