package facade

import (
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/tools"
)

// Input is one user message admitted to a session.
type Input struct {
	Content      string
	MultiContent []chat.MessagePart
	// RequestID is an optional client-chosen identity; the implementation derives the
	// TurnID from it so retries are idempotent. Empty means the implementation mints one.
	RequestID     string
	GenerateTitle bool
}

// Response resolves one outstanding interaction. Exactly one of Decision and
// Elicitation must be set, matching the interaction's kind.
type Response struct {
	InteractionID string
	Decision      *Decision
	Elicitation   *ElicitationReply
}

// DecisionType is the user's answer to a confirmation or max-iterations prompt.
type DecisionType string

const (
	DecisionApprove           DecisionType = "approve"
	DecisionApproveBalanced   DecisionType = "approve-balanced"
	DecisionApproveAutonomous DecisionType = "approve-autonomous"
	DecisionApproveTool       DecisionType = "approve-tool"
	DecisionReject            DecisionType = "reject"
)

// Decision answers a confirmation (approve/reject a tool call) or a max-iterations
// prompt (approve continues the run, reject stops it). ToolName is used by
// approve-tool; Reason by reject.
type Decision struct {
	Type     DecisionType
	Reason   string
	ToolName string
}

// ElicitationReply answers an elicitation request.
type ElicitationReply struct {
	Action  tools.ElicitationAction
	Content map[string]any
}

// Ticket identifies one accepted queue entry. ID is stable for the entry's
// lifetime. Whether an input was held or applied as guidance is reported only
// through the QueueAccepted, QueueWithdrawn and QueueConsumed events.
type Ticket struct {
	ID string
}

// PendingInput is one accepted, not yet consumed queue entry.
type PendingInput struct {
	Ticket       Ticket
	Content      string
	MultiContent []chat.MessagePart
}

// Interaction is an outstanding interaction in an observed state.
type Interaction struct {
	InteractionID string
	Kind          InteractionKind
	ElicitationID string
}

// Cursor addresses a position in one session's event sequence.
type Cursor struct {
	Epoch    string
	Sequence uint64
}

// ObservedState is the authoritative state captured atomically with an observation.
type ObservedState struct {
	Epoch        string
	Cursor       uint64
	Status       Status
	Interactions []Interaction
}

// TreeNode is one subagent node. Children nest under their parent.
type TreeNode struct {
	ID             string
	Parent         string
	SessionID      string
	Agent          string
	Task           string
	State          string
	NeedsAttention bool
	WaitingOn      string
	Error          string
	Children       []TreeNode
}

// Tree is a subagent tree snapshot. Roots is empty when the session has no subagents.
type Tree struct {
	Roots []TreeNode
}

// Todo is one todo item.
type Todo struct {
	ID          string
	Description string
	Status      string
}

// WithdrawOutcome reports the result of a Withdraw.
type WithdrawOutcome string

const (
	// WithdrawWithdrawn means the entry was removed before consumption.
	WithdrawWithdrawn WithdrawOutcome = "withdrawn"
	// WithdrawNotWithdrawn means the entry was not removed and was not known to be consumed.
	WithdrawNotWithdrawn WithdrawOutcome = "not_withdrawn"
	// WithdrawConsumed means the entry was already consumed by a turn.
	WithdrawConsumed WithdrawOutcome = "consumed"
)

// EditOutcome reports the result of a pending-input edit.
type EditOutcome string

const (
	EditEdited EditOutcome = "edited"
	// EditStale means the entry changed (or was consumed) since the caller read it.
	EditStale EditOutcome = "stale"
)
