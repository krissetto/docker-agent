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
	withdrawn := false
	err := d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped {
			return sessionIOReservation{}, ErrSessionStopped
		}
		if turnID == "" || d.sess == nil || turnID == d.activeRequestID {
			return sessionIOReservation{}, nil
		}
		queue := &d.pending
		index := slices.IndexFunc(*queue, func(msg QueuedMessage) bool { return msg.RequestID == turnID })
		if index < 0 {
			queue = &d.steering
			index = slices.IndexFunc(*queue, func(msg QueuedMessage) bool { return msg.RequestID == turnID })
		}
		if index < 0 {
			return sessionIOReservation{}, nil
		}
		msg := (*queue)[index]
		position := -1
		for i, item := range d.sess.MessagesSnapshot() {
			if item.Message != nil && item.Message.Pending && item.Message.TurnID == turnID {
				position = i
				break
			}
		}
		if position < 0 && !msg.Retry {
			return sessionIOReservation{}, nil
		}
		var write func(context.Context) error
		if msg.AcceptedPersisted {
			store, ok := d.r.sessionStore.(session.PendingMessageDeleter)
			if !ok {
				return sessionIOReservation{}, &SessionError{Kind: SessionErrorUnsupported, Operation: "cancel_pending_message"}
			}
			write = func(ctx context.Context) error { return store.DeletePendingUserMessage(ctx, d.identityID, turnID) }
		}
		d.editReserved = true
		return sessionIOReservation{write: write, commit: func(err error) error {
			d.editReserved = false
			if err != nil {
				return err
			}
			d.sess.RemovePendingUserMessageByTurnID(turnID)
			*queue = slices.DeleteFunc(*queue, func(value QueuedMessage) bool { return value.RequestID == turnID })
			d.completeTurnLocked(turnID)
			d.refreshSteeringLocked()
			d.events.PublishForRequest(d.identityID, turnID, inputEventMetadata(PendingUserMessageCanceled(d.identityID, turnID, position), msg))
			withdrawn = true
			return nil
		}}, nil
	})
	return withdrawn, err
}

func (d *sessionDriver) cancelTurn(ctx context.Context, turnID string) (CancelOutcome, error) {
	outcome := CancelNotActive
	var cancel context.CancelFunc
	err := d.ownerCall(ctx, func() error {
		if turnID != "" && d.activeRequestID == turnID && d.starting() {
			if d.startCanceled {
				outcome = CancelAlreadyCancelling
			} else {
				d.startCanceled = true
				outcome = CancelAccepted
				d.resolveInteractionsLocked()
			}
			return nil
		}
		if turnID != "" && d.activeRequestID == turnID && d.running() && d.cancel != nil {
			if d.cancelling() {
				outcome = CancelAlreadyCancelling
				return nil
			}
			d.phase = sessionCancelling
			d.resolveInteractionsLocked()
			cancel, outcome = d.cancel, CancelAccepted
		}
		return nil
	})
	if err != nil {
		return CancelNotActive, err
	}
	if outcome != CancelNotActive {
		if cancel != nil {
			cancel()
			d.refreshAttention()
		}
		return outcome, nil
	}
	withdrawn, err := d.cancelPendingMessage(ctx, turnID)
	if withdrawn {
		return CancelAccepted, err
	}
	return CancelNotActive, err
}
