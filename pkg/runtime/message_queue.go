package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

// QueuedMessage is an input waiting to be injected into the agent loop,
// either mid-turn (via the steer queue) or at end-of-turn (via the follow-up
// queue).
type QueuedMessage struct {
	InputOrigin   session.InputOrigin
	SenderID      string
	SenderName    string
	ReportOutcome session.ReportOutcome
	ID            string
	Content       string
	MultiContent  []chat.MessagePart
	// RequestID correlates session-native submissions with the events produced
	// while processing them. Legacy queue callers may leave it empty.
	RequestID string
	// AcceptedPosition is the immutable observation position reported when the
	// input was admitted. Runtime execution never uses it for identity.
	AcceptedPosition int
	// AcceptedPersisted records whether admission reached the configured session
	// store. Durable promotion is required only for such accepted inputs.
	AcceptedPersisted bool
	// Retry requests a fresh turn from existing transcript without appending
	// user input. It remains an ordinary bounded session mailbox item.
	Retry       bool
	RuntimeNote bool
	InputMode   string
}

// PendingMessageCanceler is implemented by session handles that can withdraw
// accepted input before promotion. Active-turn cancellation is separate.
type PendingMessageCanceler interface {
	CancelPendingMessage(ctx context.Context, turnID string) (bool, error)
}

// RecallHandler delivers a tool-produced message through an embedder-owned
// wake-up path. Embedders use it to resume an idle UI/session when background
// work completes after the active RunStream has stopped.
type RecallHandler func(ctx context.Context, msg QueuedMessage) bool

// MessageQueue is the interface for storing messages that are injected into
// the agent loop. Implementations must be safe for concurrent use: Enqueue
// is called from API handlers while Dequeue/Drain are called from the agent
// loop goroutine.
//
// The default implementation is NewInMemoryMessageQueue. Callers that need
// durable or distributed storage can provide their own implementation
// via the WithSteerQueue or WithFollowUpQueue options.
type MessageQueue interface {
	// Enqueue adds a message to the queue. Returns false if the queue is
	// full or the context is cancelled.
	Enqueue(ctx context.Context, msg QueuedMessage) bool
	// Dequeue removes and returns the next message from the queue.
	// Returns the message and true, or a zero value and false if the
	// queue is empty. Must not block.
	Dequeue(ctx context.Context) (QueuedMessage, bool)
	// Drain returns all pending messages and removes them from the queue.
	// Must not block — if the queue is empty it returns nil.
	Drain(ctx context.Context) []QueuedMessage
}

// NewInMemoryMessageQueue remains a compatibility constructor. Runtime inputs
// require an addressed session handle; an unaddressed legacy queue rejects input.
func NewInMemoryMessageQueue(_ int) MessageQueue { return rejectedMessageQueue{} }

type rejectedMessageQueue struct{}

func (rejectedMessageQueue) Enqueue(context.Context, QueuedMessage) bool { return false }
func (rejectedMessageQueue) Dequeue(context.Context) (QueuedMessage, bool) {
	return QueuedMessage{}, false
}
func (rejectedMessageQueue) Drain(context.Context) []QueuedMessage { return nil }

// QueueStatus is the legacy unaddressed queue projection. Session handles expose
// the authoritative addressed pending inputs instead.
type QueueStatus struct {
	SteerDepth       int
	SteerCapacity    int
	FollowupDepth    int
	FollowupCapacity int
}
