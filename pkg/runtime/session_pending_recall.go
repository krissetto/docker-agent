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
	d := h.driver
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if d.stopped {
		return false, &SessionError{Kind: SessionErrorStopped, SessionID: h.sessionID, RequestID: turnID, Operation: "cancel_pending_message"}
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
	if index < 0 || (*queue)[index].Retry {
		return false, nil
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
		return false, &SessionError{Kind: SessionErrorUnsupported, SessionID: h.sessionID, RequestID: turnID, Operation: "cancel_pending_message"}
	}
	// Child transcripts are replaced asynchronously as whole snapshots. Until
	// that writer participates in recall, do not claim durable withdrawal.
	if d.r.sessionStore != nil && d.sess.ParentID != "" {
		return unsupported()
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
	d.sess.RemovePendingUserMessageByTurnID(turnID)
	*queue = slices.Delete(*queue, index, index+1)
	d.interruptRequested = len(d.steering) != 0
	d.events.PublishForRequest(d.sess.ID, turnID, PendingUserMessageCanceled(d.sess.ID, turnID, position))
	return true, nil
}
