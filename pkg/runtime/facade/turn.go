package facade

import "context"

// Turn is one admitted turn. Its event stream and settlement are owned by the
// implementation, not by the caller's context.
type Turn interface {
	// ID is the turn identity (unique per session).
	ID() string
	// Events streams this turn's events in Seq order. It closes exactly once,
	// after settlement, once every event produced before settlement has been
	// delivered. The implementation keeps draining regardless of whether the
	// consumer reads, so settlement never waits on the consumer.
	Events() <-chan Event
	// Cancel requests cancellation. It is the only operation that cancels a turn.
	Cancel(ctx context.Context) (CancelOutcome, error)
	// Wait blocks until the turn settles and returns its outcome. Cancelling ctx
	// only abandons the wait; it never affects the turn.
	Wait(ctx context.Context) (Outcome, error)
}

// CancelOutcome reports what a Cancel request found.
type CancelOutcome string

const (
	// CancelAccepted means cancellation was requested for the running turn.
	CancelAccepted CancelOutcome = "accepted"
	// CancelAlreadyCancelling means cancellation was already in progress.
	CancelAlreadyCancelling CancelOutcome = "already_cancelling"
	// CancelNotActive means the turn is not active (it settled or never ran).
	CancelNotActive CancelOutcome = "not_active"
)

// OutcomeKind is how a turn settled.
type OutcomeKind string

const (
	OutcomeCompleted OutcomeKind = "completed"
	OutcomeCanceled  OutcomeKind = "canceled"
	OutcomeFailed    OutcomeKind = "failed"
)

// Outcome is a turn's settlement.
type Outcome struct {
	Kind OutcomeKind
	// Reason is the stop reason reported by the runtime when available (for example
	// "max_iterations"). It is empty when the runtime did not report one.
	Reason string
	// Message describes a failure. It is empty for completed and canceled turns.
	Message string
}
