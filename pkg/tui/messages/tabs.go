package messages

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/runtime"
)

// SessionRuntimeEventMsg shares the App-owned presentation head and exact
// seed provenance with each routed page.
type SessionRuntimeEventMsg struct {
	Event      runtime.Event
	Seed       bool
	Projection *lifecycle.Projection
}

// RoutedMsg wraps a message with a session ID for routing.
// Runtime events are wrapped in this type so the TUI can route
// them to the correct tab/session.
type RoutedMsg struct {
	SessionID       string  // The session ID this message is for
	RouteGeneration uint64  // Supervisor routing generation; stale deliveries are rejected
	Inner           tea.Msg // The wrapped message
}

// SpawnSessionMsg is sent when a new session should be created.
type SpawnSessionMsg struct {
	WorkingDir string // The working directory for the new session
}

// SwitchTabMsg requests switching to a different session tab.
type SwitchTabMsg struct {
	SessionID string // The session to switch to
}

// CloseTabMsg requests closing a session tab.
type CloseTabMsg struct {
	SessionID string // The session to close
}

// OpenSubagentMsg requests attaching a tab to an async subagent's
// sub-session (clicked in the sidebar swarm or on a subagent tool message).
// If a tab for that session already exists it is focused instead.
type OpenSubagentMsg struct {
	NodeID string // The subagent's tree node id
}

// ReorderTabMsg requests moving a tab from one position to another.
type ReorderTabMsg struct {
	FromIdx int
	ToIdx   int
}

// TabActivity is the presentation-only activity projected from the canonical
// session snapshot/journal, with optional descendant display folded in.
type TabActivity uint8

const (
	TabActivityNone TabActivity = iota
	TabActivityPending
	TabActivityRunning
	TabActivityDescendantRunning
)

// TabInfo contains display information for a session tab.
type TabInfo struct {
	SessionID      string      // Unique session identifier
	AgentName      string      // Current active agent name for this session
	AgentNodeID    string      // Full canonical node identity for the current agent
	Title          string      // Display title
	IsActive       bool        // Whether this is the currently active tab
	IsRunning      bool        // Whether the canonical session is starting or running
	IsAttached     bool        // Whether this tab is a live attached subagent view
	Activity       TabActivity // Presentation activity, including nested descendants
	NeedsAttention bool        // Whether the tab needs user attention (e.g., tool confirmation)
}

// TabsUpdatedMsg is sent when the tab list has changed.
type TabsUpdatedMsg struct {
	Tabs      []TabInfo
	ActiveIdx int
}

// WorkingStateChangedMsg is emitted by the content view when working state changes.
// tui.Model uses this to update the editor's working indicator and resize handle spinner.
type WorkingStateChangedMsg struct {
	Working     bool
	QueueLength int
}

// BellMsg is sent when the terminal bell should be rung to notify the user.
// This is used when an inactive tab needs attention (e.g., tool confirmation).
type BellMsg struct{}
