package facade

import (
	"context"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
)

// Queue accepts input while a turn is active.
type Queue interface {
	// Steer injects guidance into the active turn at its next safe boundary without
	// cancelling it. The boundary can arrive right after admission, so a steer is not
	// guaranteed to stay pending; use FollowUp for input that must wait for its turn.
	Steer(ctx context.Context, input Input) (Ticket, error)
	// FollowUp queues a full follow-up turn behind the active one. When nothing
	// is active the input starts immediately and no QueueAccepted event is emitted.
	FollowUp(ctx context.Context, input Input) (Ticket, error)
}

// Withdrawer removes accepted queue entries that have not been consumed.
type Withdrawer interface {
	Withdraw(ctx context.Context, ticket Ticket) (WithdrawOutcome, error)
}

// PendingInputs lists and edits accepted, not yet consumed entries.
type PendingInputs interface {
	// List reports the entries that are durably accepted and so can be edited. An
	// entry is listed only once its acceptance is durable.
	List(ctx context.Context) ([]PendingInput, error)
	// Edit replaces the content of a pending entry only if it still equals expected.
	// It reports EditStale when the entry changed or was consumed since it was read.
	Edit(ctx context.Context, ticket Ticket, expected, content string) (EditOutcome, error)
}

// Compactor compacts session history.
type Compactor interface {
	Compact(ctx context.Context, prompt string) error
}

// Pauser toggles the pause state; the result is the new state.
type Pauser interface {
	TogglePause(ctx context.Context) (bool, error)
}

// Observer reads session events with an explicit cursor. A Gap event means the
// requested cursor is no longer replayable.
type Observer interface {
	Observe(ctx context.Context, since *Cursor) (Observation, error)
}

// Observation is an open observation. Close detaches it; it never affects work.
type Observation struct {
	State  ObservedState
	Events <-chan Event
	Close  func()
}

// SubagentTreeReader reads the subagent tree.
type SubagentTreeReader interface {
	Tree(ctx context.Context) (Tree, error)
}

// SubtreeStopper stops a subagent subtree and drains it, preserving transcripts.
type SubtreeStopper interface {
	StopSubtree(ctx context.Context) error
}

// TodoEditor edits todo items and returns the resulting list.
type TodoEditor interface {
	SetStatus(ctx context.Context, id, status string) ([]Todo, error)
	SetDescription(ctx context.Context, id, expectedDescription, description string) ([]Todo, error)
	Remove(ctx context.Context, id string) ([]Todo, error)
}

// Background observes out-of-turn events. Only descendant usage and subagent status are guaranteed.
type Background interface {
	Subscribe(ctx context.Context) (Subscription, error)
}

// Subscription is an open background subscription. Close detaches it.
type Subscription struct {
	Events <-chan Event
	Close  func()
}

// Models lists and selects the models usable by the session's bound agent.
type Models interface {
	// Available lists the models the bound agent can switch to.
	Available(ctx context.Context) []modelcatalog.ModelChoice
	// Set switches the bound agent's model to the reference ref.
	Set(ctx context.Context, ref string) error
}

// Thinking reads and sets the reasoning effort the bound agent's model supports.
type Thinking interface {
	// Levels lists the supported levels; it is empty when the model has none.
	Levels(ctx context.Context) []effort.Level
	// Current reports the active level.
	Current(ctx context.Context) effort.Level
	// Set applies a level and returns the level actually applied.
	Set(ctx context.Context, level effort.Level) (effort.Level, error)
	// Cycle advances to the next supported level and returns it.
	Cycle(ctx context.Context) (effort.Level, error)
}

// AgentSwitcher changes the session's active agent in place. The SessionID, the
// Session value and its history are preserved, and the next turn runs as the new
// agent. A switch is idle-only: it fails with ErrBusy while a turn, queued input or
// an interaction is outstanding. Switching to the active agent is a no-op.
type AgentSwitcher interface {
	SwitchAgent(ctx context.Context, agentName string) error
}
