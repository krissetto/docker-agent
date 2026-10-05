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
	admission, err := d.admitInputReceipt(ctx, msg, SessionOperationPost, true, msg.trustedSteering())
	if err != nil {
		return receipt, err
	}
	receipt.Accepted, receipt.Queued, receipt.Idempotent, receipt.Durable = true, admission.queued, admission.idempotent, admission.durable
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
