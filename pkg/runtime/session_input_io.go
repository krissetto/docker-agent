package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/docker/docker-agent/pkg/session"
)

type inputAdmission struct {
	queued     bool
	idempotent bool
	durable    bool
}

func (d *sessionDriver) admitInput(ctx context.Context, msg QueuedMessage, op SessionOperation, wake, steer bool) (bool, error) {
	admission, err := d.admitInputReceipt(ctx, msg, op, wake, steer)
	return admission.queued, err
}

func (d *sessionDriver) admitInputReceipt(ctx context.Context, msg QueuedMessage, op SessionOperation, wake, steer bool) (admission inputAdmission, err error) {
	msg, err = detachQueuedInput(msg)
	if err != nil {
		return admission, err
	}
	queued := false
	err = d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.r.subagents != nil {
			if err := d.r.subagents.sessionAdmissionError(d.identityID); err != nil {
				return sessionIOReservation{}, err
			}
		}
		if found, existingQueued, existingErr := d.existingInputLocked(msg); found || existingErr != nil {
			queued = existingQueued
			admission.idempotent = found
			if found {
				for _, item := range d.sess.MessagesSnapshot() {
					if item.Message != nil && item.Message.TurnID == msg.RequestID {
						admission.durable = item.Message.ID != 0
						break
					}
				}
				for _, queue := range [][]QueuedMessage{d.pending, d.steering} {
					for _, existing := range queue {
						if existing.RequestID == msg.RequestID {
							admission.durable = existing.AcceptedPersisted
						}
					}
				}
			}
			return sessionIOReservation{commit: func(error) error { return existingErr }}, nil
		}
		if !d.stopped && !d.running() && !d.starting() && !wake {
			return sessionIOReservation{}, ErrSessionStopped
		}
		if err := d.admitLocked(op); err != nil {
			err.RequestID = msg.RequestID
			return sessionIOReservation{}, err
		}
		queued = !steer || d.compactReserved || (len(d.pending) != 0 && (!d.running() || d.activeRequestID == ""))
		count := len(d.steering)
		if queued {
			count = len(d.pending)
		}
		if !limitAllows(count, d.pendingLimit()) {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorCapacity, SessionID: d.identityID, RequestID: msg.RequestID, Operation: op, Reason: SessionErrorReasonLimit, Limit: d.pendingLimit()}
		}
		message := msg.sessionMessage()
		msg.InputMode = message.InputMode
		message.Pending, message.Accepted, message.TurnID = true, true, msg.RequestID
		message.Message.CreatedAt = ""
		var write func(context.Context) error
		if !msg.Retry && msg.RequestID != "" && d.r.sessionStore != nil {
			write = func(ctx context.Context) error {
				var id int64
				var err error
				if appender, ok := d.r.sessionStore.(session.ItemAppender); ok {
					id, err = appender.AppendItem(ctx, d.identityID, "input:"+msg.RequestID, session.NewMessageItem(message))
				} else {
					id, err = d.r.sessionStore.AddMessage(ctx, d.identityID, message)
				}
				if errors.Is(err, session.ErrNotFound) {
					return nil
				}
				if err == nil {
					msg.AcceptedPersisted = true
					message.ID = id
				}
				return err
			}
		}
		return sessionIOReservation{write: write, commit: func(err error) error {
			if err != nil {
				return err
			}
			if d.r.subagents != nil {
				if err := d.r.subagents.sessionAdmissionError(d.identityID); err != nil {
					return err
				}
			}
			admission.durable = msg.AcceptedPersisted
			if !msg.Retry && msg.RequestID != "" {
				msg.AcceptedPosition = d.sess.AddMessageAt(message)
				d.events.PublishForRequest(d.identityID, msg.RequestID, inputEventMetadata(PendingUserMessageAccepted(d.identityID, msg.RequestID, msg.Content, msg.MultiContent, msg.AcceptedPosition), msg))
			} else if msg.Retry {
				if d.recentRetries == nil {
					d.recentRetries = map[string]bool{}
				}
				d.recentRetries[msg.RequestID] = true
				if len(d.recentRetries) > defaultMaxSubagentMailbox {
					for id := range d.recentRetries {
						if id != d.activeRequestID && id != msg.RequestID {
							delete(d.recentRetries, id)
							break
						}
					}
				}
			}
			if queued {
				d.pending = append(d.pending, msg)
			} else {
				d.steering = append(d.steering, msg)
			}
			if normalizedInputOrigin(msg.InputOrigin) == session.InputOriginUser {
				d.authorizeViewLocked()
			}
			d.refreshSteeringLocked()
			return nil
		}}, nil
	})
	admission.queued = queued
	return admission, err
}

func (d *sessionDriver) promoteInput(ctx context.Context, turnID string, consume func(QueuedMessage)) error {
	return d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.r.subagents != nil {
			if err := d.r.subagents.sessionAdmissionError(d.identityID); err != nil {
				return sessionIOReservation{}, err
			}
		}
		if d.stopped {
			return sessionIOReservation{}, ErrSessionStopped
		}
		var msg QueuedMessage
		found := false
		for _, queue := range [][]QueuedMessage{d.pending, d.steering} {
			for _, value := range queue {
				if value.RequestID == turnID {
					msg, found = value, true
					break
				}
			}
		}
		if !found {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorStale, Operation: "promote_input"}
		}
		var write func(context.Context) error
		if msg.AcceptedPersisted && !msg.Retry {
			write = func(ctx context.Context) error {
				return d.r.sessionStore.PromotePendingUserMessage(ctx, d.identityID, turnID)
			}
		}
		return sessionIOReservation{write: write, commit: func(err error) error {
			if err != nil {
				d.lastError = err.Error()
				d.publishPromotionFailureLocked(turnID, err)
				return err
			}
			if !msg.Retry {
				if !d.sess.PromotePendingUserMessageByTurnID(turnID) {
					return &SessionError{Kind: SessionErrorStale, Operation: "promote_input"}
				}
				if msg.InputMode == "steer" || msg.trustedSteering() {
					d.consumedSteering = append(d.consumedSteering, turnID)
				}
				d.lastFailureKey = ""
				d.events.PublishForRequest(d.identityID, turnID, inputEventMetadata(PendingUserMessagePromoted(d.identityID, turnID, msg.Content, msg.MultiContent, msg.AcceptedPosition), msg))
			}
			if consume != nil {
				consume(msg)
			}
			return nil
		}}, nil
	})
}

func (d *sessionDriver) drainInputs(ctx context.Context, steeringOnly bool) []QueuedMessage {
	var batch []QueuedMessage
	_ = d.ownerCall(ctx, func() error {
		batch = slices.Clone(d.steering)
		if !steeringOnly {
			for _, msg := range d.pending {
				if msg.RequestID == "" || msg.trustedSteering() {
					batch = append(batch, msg)
				}
			}
		}
		slices.SortStableFunc(batch, func(a, b QueuedMessage) int { return a.AcceptedPosition - b.AcceptedPosition })
		return nil
	})
	var drained []QueuedMessage
	consume := func(msg QueuedMessage) {
		remove := func(queued QueuedMessage) bool {
			return queued.RequestID == msg.RequestID && queued.Content == msg.Content && queued.AcceptedPosition == msg.AcceptedPosition
		}
		d.steering = slices.DeleteFunc(d.steering, remove)
		d.pending = slices.DeleteFunc(d.pending, remove)
		drained = append(drained, msg)
		d.refreshSteeringLocked()
		d.notifyTurnChangedLocked()
	}
	for _, msg := range batch {
		if msg.RequestID == "" {
			if err := d.ownerCall(ctx, func() error { consume(msg); return nil }); err != nil {
				break
			}
		} else if err := d.promoteInput(ctx, msg.RequestID, consume); err != nil {
			break
		}
	}
	return drained
}

// A canceled start becomes a settling generation without launching execution.
// Ordinary settlement retries retain its canceled outcome on storage failure.
func (d *sessionDriver) finishCanceledStart() bool {
	var canceled bool
	var generation uint64
	_ = d.ownerCall(d.r.durabilityContext(), func() error {
		if !d.starting() || !d.startCanceled {
			return nil
		}
		canceled = true
		turnID := d.activeRequestID
		d.pending = slices.DeleteFunc(d.pending, func(msg QueuedMessage) bool { return msg.RequestID == turnID })
		d.generation++
		generation = d.generation
		d.phase, d.canceledOutcome, d.startCanceled = sessionSettling, true, false
		d.events.SetRequest(d.identityID, turnID, generation)
		d.signalStartDoneLocked()
		return nil
	})
	if canceled {
		ctx, next, again := d.finishRun(generation, "")
		if again {
			d.wg.Add(1)
			d.startWake(ctx, next)
		}
	}
	return canceled
}

func detachQueuedInput(msg QueuedMessage) (QueuedMessage, error) {
	data, err := json.Marshal(msg.MultiContent)
	if err != nil {
		return QueuedMessage{}, err
	}
	msg.MultiContent = nil
	err = json.Unmarshal(data, &msg.MultiContent)
	return msg, err
}
