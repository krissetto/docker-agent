package runtime

import (
	"context"
	"slices"

	"github.com/docker/docker-agent/pkg/session"
)

// PendingUserMessageCanceledEvent withdraws an accepted input without promoting
// it into model context. Projections remove the pending entry by TurnID.
type PendingUserMessageCanceledEvent struct {
	AgentContext

	InputOrigin   session.InputOrigin   `json:"input_origin,omitempty"`
	SenderID      string                `json:"sender_id,omitempty"`
	SenderName    string                `json:"sender_name,omitempty"`
	ReportOutcome session.ReportOutcome `json:"report_outcome,omitempty"`
	InputMode     string                `json:"input_mode,omitempty"`

	Type            string `json:"type"`
	SessionID       string `json:"session_id"`
	TurnID          string `json:"turn_id"`
	SessionPosition int    `json:"session_position"`
}

func PendingUserMessageCanceled(sessionID, turnID string, position int) Event {
	return &PendingUserMessageCanceledEvent{Type: "pending_user_message_canceled", SessionID: sessionID, TurnID: turnID, SessionPosition: position, AgentContext: newAgentContext("")}
}

func (e *PendingUserMessageCanceledEvent) GetSessionID() string { return e.SessionID }

var _ PendingMessageCanceler = (*sessionHandle)(nil)

// CancelPendingMessage linearizes withdrawal with both FIFO and steering
// promotion. A failed durable deletion leaves the mailbox and transcript intact.
// Cancel remains the separate active-turn-only operation.
func (h *sessionHandle) CancelPendingMessage(ctx context.Context, turnID string) (bool, error) {
	return h.driver.cancelPendingMessage(ctx, turnID)
}

func (d *sessionDriver) cancelPendingMessage(ctx context.Context, turnID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancelPendingLocked(ctx, turnID)
}

func (d *sessionDriver) cancelPendingLocked(ctx context.Context, turnID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if d.stopped {
		return false, &SessionError{Kind: SessionErrorStopped, SessionID: d.sessionIDLocked(), RequestID: turnID, Operation: "cancel_pending_message"}
	}
	if turnID == "" || d.sess == nil || turnID == d.activeRequestID {
		return false, nil
	}
	queue := &d.pending
	index := slices.IndexFunc(*queue, func(msg QueuedMessage) bool { return msg.RequestID == turnID })
	if index < 0 {
		queue = &d.steering
		index = slices.IndexFunc(*queue, func(msg QueuedMessage) bool { return msg.RequestID == turnID })
	}
	if index < 0 {
		return false, nil
	}
	if (*queue)[index].Retry {
		*queue = slices.Delete(*queue, index, index+1)
		d.completeTurnLocked(turnID)
		return true, nil
	}
	position := -1
	for i, item := range d.sess.MessagesSnapshot() {
		if item.Message != nil && item.Message.Pending && item.Message.TurnID == turnID {
			position = i
			break
		}
	}
	if position < 0 {
		return false, nil
	}
	unsupported := func() (bool, error) {
		return false, &SessionError{Kind: SessionErrorUnsupported, SessionID: d.sessionIDLocked(), RequestID: turnID, Operation: "cancel_pending_message"}
	}
	if (*queue)[index].AcceptedPersisted {
		store, ok := d.r.sessionStore.(session.PendingMessageDeleter)
		if !ok {
			return unsupported()
		}
		if err := store.DeletePendingUserMessage(ctx, d.sess.ID, turnID); err != nil {
			return false, err
		}
	}
	// The in-memory store may share the session pointer and have already
	// removed this item. All remaining operations are infallible under d.mu.
	msg := (*queue)[index]
	d.sess.RemovePendingUserMessageByTurnID(turnID)
	*queue = slices.Delete(*queue, index, index+1)
	d.completeTurnLocked(turnID)
	d.refreshSteeringLocked()
	d.events.PublishForRequest(d.sess.ID, turnID, inputEventMetadata(PendingUserMessageCanceled(d.sess.ID, turnID, position), msg))
	return true, nil
}

func (d *sessionDriver) cancelTurn(ctx context.Context, turnID string) (CancelOutcome, error) {
	d.mu.Lock()
	if err := ctx.Err(); err != nil {
		d.mu.Unlock()
		return CancelNotActive, err
	}
	if turnID != "" && d.activeRequestID == turnID && d.running() && d.cancel != nil {
		if d.cancelling() {
			d.mu.Unlock()
			return CancelAlreadyCancelling, nil
		}
		d.phase = sessionCancelling
		d.resolveInteractionsLocked()
		cancel := d.cancel
		d.mu.Unlock()
		cancel()
		d.refreshAttention()
		return CancelAccepted, nil
	}
	withdrawn, err := d.cancelPendingLocked(ctx, turnID)
	d.mu.Unlock()
	if err != nil {
		return CancelNotActive, err
	}
	if withdrawn {
		return CancelAccepted, nil
	}
	return CancelNotActive, nil
}
