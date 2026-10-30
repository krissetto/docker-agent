package facade

import (
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

// Event is an envelope around exactly one Payload. Seq is assigned from the
// source sequence: it never decreases within a session, may skip values, and
// events derived from the same source record share it.
type Event struct {
	SessionID string
	// TurnID is set for turn-scoped events and empty for out-of-band events.
	TurnID string
	Seq    uint64
	// Epoch identifies the sequence space that Seq belongs to.
	Epoch string
	// MessageID is set only when the runtime provides it. It is never synthesized.
	MessageID string
	Payload   Payload
}

// Payload is one of the seam-owned lifecycle values or a Passthrough.
type Payload interface{ isPayload() }

// TurnStarted marks the start of the turn named by Event.TurnID.
type TurnStarted struct{}

// TurnSettled marks settlement of the turn named by Event.TurnID. It is the last event of a turn.
type TurnSettled struct{ Outcome Outcome }

// InteractionKind identifies what an interaction asks for.
type InteractionKind string

const (
	InteractionConfirmation  InteractionKind = "confirmation"
	InteractionMaxIterations InteractionKind = "max_iterations"
	InteractionElicitation   InteractionKind = "elicitation"
)

// InteractionRequested asks the consumer to Respond. Exactly one of the detail
// fields is set, matching Kind.
type InteractionRequested struct {
	InteractionID string
	Kind          InteractionKind
	Confirmation  *Confirmation
	// MaxIterations is the configured limit that was reached.
	MaxIterations int
	Elicitation   *ElicitationRequest
}

// Confirmation describes a tool call awaiting approval.
type Confirmation struct {
	ToolCall tools.ToolCall
	Tool     tools.Tool
	Metadata map[string]string
}

// ElicitationRequest describes a prompt for structured input.
type ElicitationRequest struct {
	ElicitationID string
	Mode          string
	Message       string
	URL           string
	Schema        any
	Meta          map[string]any
}

// InteractionResolution is how an interaction was closed.
type InteractionResolution string

const (
	ResolutionResponded InteractionResolution = "responded"
	ResolutionCanceled  InteractionResolution = "canceled"
	ResolutionStopped   InteractionResolution = "stopped"
)

// InteractionResolved closes an interaction exactly once, however it was closed.
type InteractionResolved struct {
	InteractionID string
	Resolution    InteractionResolution
}

// QueueAccepted reports that an input was accepted into the queue.
type QueueAccepted struct{ Ticket Ticket }

// QueueWithdrawn reports that a queued input was removed before consumption.
type QueueWithdrawn struct{ Ticket Ticket }

// QueueConsumed reports that a queued input was consumed by a turn.
type QueueConsumed struct{ Ticket Ticket }

// SubagentStatus reports a subagent's lifecycle state.
type SubagentStatus struct {
	ChildSessionID  string
	ParentSessionID string
	Agent           string
	// State uses the subagent lifecycle vocabulary: starting, running, idle, completed, failed, stopped.
	State string
}

// Gap reports that events before FirstAvailable were discarded. Consumers must
// discard their projection and re-observe without a cursor.
type Gap struct{ FirstAvailable uint64 }

// Passthrough carries a runtime event unchanged. It is read-only; consumers
// must tolerate runtime event types that another implementation never emits.
type Passthrough struct{ Event runtime.Event }

func (TurnStarted) isPayload()          {}
func (TurnSettled) isPayload()          {}
func (InteractionRequested) isPayload() {}
func (InteractionResolved) isPayload()  {}
func (QueueAccepted) isPayload()        {}
func (QueueWithdrawn) isPayload()       {}
func (QueueConsumed) isPayload()        {}
func (SubagentStatus) isPayload()       {}
func (Gap) isPayload()                  {}
func (Passthrough) isPayload()          {}
