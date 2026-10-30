// Package facade is the consumer-side typed boundary over a session runtime.
//
// The core (Host, Session, Turn, Event) is mandatory and truthful on every
// implementation. Optional behaviour is exposed only through capability
// accessors that return (impl, true) when the implementation supports the
// capability and (nil, false) otherwise. An implementation never stubs,
// no-ops or fakes an unsupported capability.
//
// The core depends only on runtime-neutral value packages and the
// runtime.Event interface, so any session runtime can implement it through an
// adapter without changing this package.
//
// Ownership: a context passed to Session.Run or to an observation scopes only
// admission or observation. Cancelling it detaches the caller and never cancels
// accepted work. Turn.Cancel is the only operation that cancels a turn.
package facade

import (
	"context"
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

// Host is the session registry a consumer works against.
type Host interface {
	// Create admits a new session bound to an agent and returns its executable handle.
	Create(ctx context.Context, options CreateOptions) (Session, error)
	// Open loads or restores an existing session for execution.
	Open(ctx context.Context, sessionID string) (Session, error)
	// List returns one page of session summaries, continuing after Cursor.
	List(ctx context.Context, options ListOptions) (Page, error)
	// Delete removes a session and its persisted state.
	Delete(ctx context.Context, sessionID string) error
	// Close releases only the resources this host acquired. It never shuts down a borrowed runtime.
	Close(ctx context.Context) error
}

// Session is one executable session. Its accessors report optional capabilities.
// Info and the accessors are evaluated at each call, so an agent switch or a model
// change is visible immediately.
type Session interface {
	// Info reports the session's current identity and the capabilities it exposes.
	Info() Info
	// Status reports a point-in-time reading of activity.
	Status(ctx context.Context) (Status, error)
	// Run admits one turn. The returned Turn owns the turn's event stream and settlement.
	Run(ctx context.Context, input Input) (Turn, error)
	// Respond resolves one outstanding interaction by InteractionID.
	Respond(ctx context.Context, response Response) error
	// Snapshot returns a clone of the session state. Callers may retain and mutate it.
	Snapshot(ctx context.Context) (*session.Session, error)
	// Close releases resources this Session acquired (observation feeds). It never cancels work.
	Close(ctx context.Context) error

	// Queue accepts steering and follow-up input while a turn is active.
	Queue() (Queue, bool)
	// Withdraw cancels an accepted, not yet consumed queue ticket.
	Withdraw() (Withdrawer, bool)
	// Pending lists and edits accepted, not yet consumed inputs.
	Pending() (PendingInputs, bool)
	// Compact runs manual compaction of the session history.
	Compact() (Compactor, bool)
	// Pause toggles the session's pause state.
	Pause() (Pauser, bool)
	// Observer observes session events from a cursor, reporting gaps explicitly.
	Observer() (Observer, bool)
	// SubagentTree reads the session's subagent tree.
	SubagentTree() (SubagentTreeReader, bool)
	// StopSubtree stops and drains a subagent subtree while preserving transcripts.
	StopSubtree() (SubtreeStopper, bool)
	// TodoEditor edits todo items.
	TodoEditor() (TodoEditor, bool)
	// Background subscribes to out-of-turn events: descendant usage and subagent status only.
	Background() (Background, bool)
	// Models lists and selects the models the bound agent can use.
	Models() (Models, bool)
	// Thinking reads and sets the reasoning effort of the bound agent's model.
	Thinking() (Thinking, bool)
	// AgentSwitch changes the session's active agent in place, keeping its identity and history.
	AgentSwitch() (AgentSwitcher, bool)
}

// Info is the identity of a session plus the capabilities it exposes right now.
type Info struct {
	SessionID     string
	AgentName     string
	Model         string
	ThinkingLevel string
	Capabilities  Capabilities
}

// Capabilities reports which optional capabilities a Session exposes. It must
// agree exactly with the accessors on Session.
type Capabilities struct {
	Queue        bool
	Withdraw     bool
	Pending      bool
	Compact      bool
	Pause        bool
	Observer     bool
	SubagentTree bool
	StopSubtree  bool
	TodoEditor   bool
	Background   bool
	Models       bool
	Thinking     bool
	AgentSwitch  bool
}

// Status is a point-in-time reading.
type Status struct {
	Busy bool
	// Paused reports a pause the run has reached at an iteration boundary.
	Paused bool
	// PauseArmed reports a requested pause, which the run reaches at its next boundary.
	PauseArmed    bool
	PendingCount  int
	CurrentTurnID string
}

// CreateOptions describes a session to create.
type CreateOptions struct {
	AgentName  string
	Model      string
	WorkingDir string
}

// ListOptions selects a page of session summaries.
type ListOptions struct {
	WorkingDir string
	// Cursor is opaque and comes from Page.Next; empty starts at the beginning.
	Cursor string
	// Limit bounds the page size; zero selects the implementation default.
	Limit int
}

// Summary is a metadata-only session row.
type Summary struct {
	SessionID   string
	AgentName   string
	Title       string
	WorkingDir  string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	NumMessages int
	Cost        float64
	Starred     bool
	Loadable    bool
}

// Page is one page of summaries. Next is empty when the listing is exhausted.
type Page struct {
	Sessions []Summary
	Next     string
}
