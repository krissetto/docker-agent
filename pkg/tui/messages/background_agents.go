package messages

// BackgroundAgentStatusMsg reports that a detached (background) agent's child
// session started or stopped running. The sidebar counts running background
// agents by child session. The producer is the session-bound Background
// subscription: its SubagentStatus events map to Running with the states
// starting and running counted as running, and idle or terminal states as not.
type BackgroundAgentStatusMsg struct {
	ChildSessionID string
	AgentName      string
	Running        bool
}
