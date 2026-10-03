package runtime

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// postCommunication acknowledges admission only; waking and model consumption
// are deliberately outside the commit point.
func (d *sessionDriver) postCommunication(ctx context.Context, msg QueuedMessage) (subagent.DeliveryReceipt, error) {
	receipt := subagent.DeliveryReceipt{RequestID: msg.RequestID, Target: d.sessionID()}
	if msg.trustedSteering() {
		receipt.Disposition = subagent.DeliveryGuidance
	} else {
		receipt.Disposition = subagent.DeliveryNewTurn
	}
	d.mu.Lock()
	if found, queued, err := d.existingInputLocked(msg); found || err != nil {
		if err == nil {
			receipt.Accepted, receipt.Idempotent, receipt.Queued = true, true, queued
			for _, pending := range d.pending {
				if pending.RequestID == msg.RequestID {
					receipt.Durable = pending.AcceptedPersisted
					break
				}
			}
		}
		d.mu.Unlock()
		return receipt, err
	}
	if err := ctx.Err(); err != nil {
		d.mu.Unlock()
		return receipt, err
	}
	if err := d.admitLocked(SessionOperationPost); err != nil {
		err.RequestID = msg.RequestID
		d.mu.Unlock()
		return receipt, err
	}
	if !limitAllows(len(d.pending), d.pendingLimit()) {
		err := &SessionError{Kind: SessionErrorCapacity, SessionID: d.sessionIDLocked(), RequestID: msg.RequestID, Operation: SessionOperationPost, Reason: SessionErrorReasonLimit, Limit: d.pendingLimit()}
		d.mu.Unlock()
		return receipt, err
	}
	if err := d.acceptInputLocked(&msg); err != nil {
		d.mu.Unlock()
		return receipt, err
	}
	d.pending = append(d.pending, msg)
	if msg.trustedSteering() {
		d.refreshSteeringLocked()
	}
	receipt.Accepted, receipt.Queued, receipt.Durable = true, true, msg.AcceptedPersisted
	d.mu.Unlock()
	d.refreshAttention()
	d.WakePending()
	return receipt, nil
}

func communicationRejection(receipt subagent.DeliveryReceipt, err error) subagent.DeliveryReceipt {
	if err == nil {
		return receipt
	}
	if receipt.Rejection == "" {
		receipt.Rejection = "unavailable"
		if sessionErr, ok := errors.AsType[*SessionError](err); ok {
			receipt.Rejection = string(sessionErr.Kind)
		} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			receipt.Rejection = "canceled"
		}
	}
	receipt.Detail = err.Error()
	return receipt
}

func (m *subagentManager) postAgentCommunication(ctx context.Context, target string, msg QueuedMessage) (subagent.DeliveryReceipt, error) {
	d, ok := m.r.sessionDrivers.Lookup(target)
	if !ok {
		// A volatile orphan buffer cannot acknowledge durable inbox admission.
		return subagent.DeliveryReceipt{RequestID: msg.RequestID, Target: target}, &SessionError{Kind: SessionErrorNotFound, SessionID: target, RequestID: msg.RequestID, Operation: SessionOperationPost, Detail: "recipient has no live inbox; message was not accepted"}
	}
	return d.postCommunication(ctx, msg)
}

func agentCommunication(body, requestID, senderID, senderName string, mode subagent.DeliveryMode) QueuedMessage {
	input := "steer"
	if mode == subagent.DeliveryNewTurn {
		input = "turn"
	}
	return QueuedMessage{Content: body, RequestID: requestID, InputOrigin: session.InputOriginAgent, SenderID: senderID, SenderName: senderName, InputMode: input}
}
