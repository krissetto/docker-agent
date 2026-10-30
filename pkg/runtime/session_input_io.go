package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	"github.com/docker/docker-agent/pkg/session"
)

type inputAdmission struct {
	queued           bool
	idempotent       bool
	durable          bool
	deferredAccepted bool
	startOwner       bool
}

type deferredAcceptedInput struct {
	event *PendingUserMessageAcceptedEvent
	owner *byte
}

func (d *sessionDriver) admitInput(ctx context.Context, msg QueuedMessage, op SessionOperation, wake bool) (bool, error) {
	return d.admitInputWithDeferredAccepted(ctx, msg, op, wake, false)
}

func (d *sessionDriver) admitInputForStart(ctx context.Context, msg QueuedMessage, op SessionOperation, wake, deferIdleAccepted bool, token *byte) (inputAdmission, error) {
	return d.admitInputReceiptWithDeferredAccepted(ctx, msg, op, wake, deferIdleAccepted, token)
}

func (d *sessionDriver) admitInputWithDeferredAccepted(ctx context.Context, msg QueuedMessage, op SessionOperation, wake, deferIdleAccepted bool) (bool, error) {
	admission, err := d.admitInputReceiptWithDeferredAccepted(ctx, msg, op, wake, deferIdleAccepted, nil)
	return admission.queued, err
}

func (d *sessionDriver) admitInputReceipt(ctx context.Context, msg QueuedMessage, op SessionOperation, wake bool) (inputAdmission, error) {
	return d.admitInputReceiptWithDeferredAccepted(ctx, msg, op, wake, false, nil)
}

func (d *sessionDriver) admitInputReceiptWithDeferredAccepted(ctx context.Context, msg QueuedMessage, op SessionOperation, wake, deferIdleAccepted bool, token *byte) (inputAdmission, error) {
	return d.admitInputReceiptWithAuthorization(ctx, msg, op, wake, deferIdleAccepted, token, false)
}

func (d *sessionDriver) admitInputReceiptWithAuthorization(ctx context.Context, msg QueuedMessage, op SessionOperation, wake, deferIdleAccepted bool, token *byte, authorizeDormant bool) (inputAdmission, error) {
	var admission inputAdmission
	msg, err := detachQueuedInput(msg)
	if err != nil {
		return admission, err
	}
	if err := d.ownerCall(ctx, func() error {
		if d.switchReserved {
			return &SessionError{Kind: SessionErrorCapacity, SessionID: d.identityID, RequestID: msg.RequestID, Operation: SessionOperationSwitchAgent, Reason: SessionErrorReasonBusy}
		}
		return nil
	}); err != nil {
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
			if authorizeDormant && normalizedInputOrigin(msg.InputOrigin) != session.InputOriginUser && d.viewDormant && !d.stopped && existingErr == nil {
				wakesDormant := !found
				for _, queued := range d.pending {
					if queued.RequestID == msg.RequestID && !queued.Retry {
						wakesDormant = true
						break
					}
				}
				if wakesDormant {
					if err := d.dormantCommunicationAdmissionLocked(msg); err != nil {
						return sessionIOReservation{}, err
					}
				}
			}
			queued = existingQueued
			admission.idempotent = found
			if deferred, exists := d.deferredAccepted[msg.RequestID]; exists {
				admission.deferredAccepted = true
				admission.startOwner = token != nil && deferred.owner == token
			}
			if found && authorizeDormant && d.viewDormant && !d.stopped && !msg.Retry && msg.RequestID != "" {
				pendingAccepted := false
				for _, queued := range d.pending {
					if queued.RequestID == msg.RequestID && !queued.Retry {
						pendingAccepted = true
						break
					}
				}
				if pendingAccepted {
					for _, item := range d.sess.MessagesSnapshot() {
						if item.Message != nil && item.Message.TurnID == msg.RequestID && item.Message.Pending && item.Message.Accepted {
							d.authorizeViewLocked()
							break
						}
					}
				}
			}
			if found {
				for _, item := range d.sess.MessagesSnapshot() {
					if item.Message != nil && item.Message.TurnID == msg.RequestID {
						admission.durable = item.Message.ID != 0
						break
					}
				}
				for _, existing := range d.pending {
					if existing.RequestID == msg.RequestID {
						admission.durable = existing.AcceptedPersisted
					}
				}
			}
			return sessionIOReservation{commit: func(error) error { return existingErr }}, nil
		}
		if authorizeDormant && d.viewDormant && !d.stopped {
			if err := d.dormantCommunicationAdmissionLocked(msg); err != nil {
				return sessionIOReservation{}, err
			}
		}
		if !d.stopped && !d.running() && !d.starting() && !wake {
			return sessionIOReservation{}, ErrSessionStopped
		}
		if err := d.admitLocked(op); err != nil {
			err.RequestID = msg.RequestID
			return sessionIOReservation{}, err
		}
		if msg.Retry {
			// An explicit retry is the only way to resume after an uncertain write.
			d.unidentifiedUncertain = false
		}
		queued = d.queuedAtAdmissionLocked(msg)
		deferAccepted := msg.RequestID != "" && !msg.Retry &&
			(len(d.deferredAcceptedOrder) > 0 || (deferIdleAccepted && wake && ctx.Err() == nil && d.phase == sessionIdle && len(d.pending) == 0 && !d.compactReserved && !d.viewDormant))
		count := len(d.pending)
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
				if err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					if appender, ok := d.r.sessionStore.(session.ItemAppender); ok {
						reconcileCtx, cancel := context.WithTimeout(d.r.durabilityContext(), defaultSubagentPersistenceTimeout)
						id, err = appender.AppendItem(reconcileCtx, d.identityID, "input:"+msg.RequestID, session.NewMessageItem(message))
						cancel()
					}
				}
				if errors.Is(err, session.ErrInputWithdrawn) {
					return &SessionError{Kind: SessionErrorConflict, SessionID: d.identityID, RequestID: msg.RequestID, Operation: op, Detail: "input identity was withdrawn"}
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
				if write != nil {
					if d.ioLane.uncertainInputs == nil {
						d.ioLane.uncertainInputs = make(map[string]bool)
					}
					d.ioLane.uncertainInputs[msg.RequestID] = true
				}
				return err
			}
			delete(d.ioLane.uncertainInputs, msg.RequestID)
			admission.durable = msg.AcceptedPersisted
			// The append receipt remains authoritative even after execution is fenced.
			if !msg.Retry && msg.RequestID != "" {
				msg.AcceptedPosition = d.sess.AddMessageAt(message)
			}
			if d.r.subagents != nil {
				if err := d.r.subagents.sessionAdmissionError(d.identityID); err != nil {
					return err
				}
			}
			if msg.RequestID != "" {
				if d.inputFingerprints == nil {
					d.inputFingerprints = make(map[string]string)
				}
				data, _ := json.Marshal(msg.sessionMessage())
				hash := sha256.Sum256(data)
				d.inputFingerprints[msg.RequestID] = hex.EncodeToString(hash[:])
			}
			if !msg.Retry && msg.RequestID != "" {
				event := inputEventMetadata(PendingUserMessageAccepted(d.identityID, msg.RequestID, msg.Content, msg.MultiContent, msg.AcceptedPosition), msg).(*PendingUserMessageAcceptedEvent)
				if deferAccepted {
					if d.deferredAccepted == nil {
						d.deferredAccepted = make(map[string]deferredAcceptedInput)
					}
					d.deferredAccepted[msg.RequestID] = deferredAcceptedInput{event: event, owner: token}
					d.deferredAcceptedOrder = append(d.deferredAcceptedOrder, msg.RequestID)
					admission.deferredAccepted = true
					admission.startOwner = true
				} else {
					d.events.PublishForRequest(d.identityID, msg.RequestID, event)
				}
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
			d.pending = append(d.pending, msg)
			if authorizeDormant {
				d.authorizeViewLocked()
			}
			d.refreshSteeringLocked()
			return nil
		}}, nil
	})
	if err != nil {
		// A canceled caller must not read acknowledgement-owned result storage.
		return inputAdmission{}, err
	}
	admission.queued = queued
	return admission, nil
}

// dormantCommunicationAdmissionLocked permits a directed send to resume only
// the previously accepted durable FIFO. Restored legacy pending transcript rows
// without admission evidence remain visible but cannot hitchhike on new work.
func (d *sessionDriver) dormantCommunicationAdmissionLocked(msg QueuedMessage) error {
	pendingAccepted := make(map[string]bool, len(d.pending)+1)
	for _, queued := range d.pending {
		if queued.Retry || queued.RequestID == "" {
			continue
		}
		pendingAccepted[queued.RequestID] = false
	}
	for _, item := range d.sess.MessagesSnapshot() {
		if item.Message == nil || !item.Message.Pending {
			continue
		}
		if !item.Message.Accepted {
			return &SessionError{Kind: SessionErrorInvalid, SessionID: d.identityID, RequestID: msg.RequestID, Operation: SessionOperationSend, Detail: "restored inbox contains an unaccepted legacy pending message; explicit session resume is required"}
		}
		if _, queued := pendingAccepted[item.Message.TurnID]; !queued {
			return &SessionError{Kind: SessionErrorPersistence, SessionID: d.identityID, RequestID: item.Message.TurnID, Operation: SessionOperationSend, Detail: "restored accepted input lacks a live durable mailbox entry"}
		}
		pendingAccepted[item.Message.TurnID] = true
	}
	for id, accepted := range pendingAccepted {
		if !accepted {
			return &SessionError{Kind: SessionErrorPersistence, SessionID: d.identityID, RequestID: id, Operation: SessionOperationSend, Detail: "restored accepted mailbox input is missing durable pending evidence"}
		}
	}
	return nil
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
		for _, value := range d.pending {
			if value.RequestID == turnID {
				msg, found = value, true
				break
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
				d.publishDeferredAcceptedLocked(turnID, true)
				if msg.guidance() {
					d.consumedSteering = append(d.consumedSteering, turnID)
				}
				d.lastFailureKey = ""
				d.events.PublishForRequest(d.identityID, turnID, inputEventMetadata(PendingUserMessagePromoted(d.identityID, turnID, msg.Content, msg.MultiContent, msg.AcceptedPosition), msg))
				d.flushAllDeferredAcceptedLocked()
			}
			if consume != nil {
				consume(msg)
			}
			return nil
		}}, nil
	})
}

// promoteUnidentifiedInput moves the queue head that has no request ID into the
// transcript. Its durable append uses the existing AddMessage path; the commit
// appends the transcript entry and removes the head only after the write result
// is known. A failed write leaves the queue untouched, and the next wake retries
// the append. An uncertain write that did commit before failing can therefore be
// duplicated, because there is no row identity to deduplicate on. That residual
// window is documented, not closed. Retry entries carry no content and only
// consume the head.
func (d *sessionDriver) promoteUnidentifiedInput(ctx context.Context, head QueuedMessage) (int, error) {
	position := -1
	err := d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped {
			return sessionIOReservation{}, ErrSessionStopped
		}
		if d.unidentifiedUncertain && !head.Retry {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorInvalid, Operation: "promote_input", Detail: "uncertain input write requires an explicit retry"}
		}
		if len(d.pending) == 0 || !sameUnidentifiedInput(d.pending[0], head) {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorStale, Operation: "promote_input"}
		}
		// The fence captures the queue epoch at reservation. A stop or generation
		// change in between means the head at commit may be a different admission
		// with identical fields, so it must never be consumed by this write.
		generation, stopped, stopRequest := d.generation, d.stopped, d.stopRequest.Load()
		fenced := func() bool {
			return d.generation != generation || d.stopped != stopped || d.stopRequest.Load() != stopRequest
		}
		consumeHead := func() {
			d.pending = d.pending[1:]
			d.refreshSteeringLocked()
			d.notifyTurnChangedLocked()
		}
		if head.Retry {
			return sessionIOReservation{commit: func(err error) error {
				if err != nil {
					return err
				}
				if fenced() || len(d.pending) == 0 || !sameUnidentifiedInput(d.pending[0], head) {
					return &SessionError{Kind: SessionErrorStale, Operation: "promote_input"}
				}
				consumeHead()
				return nil
			}}, nil
		}
		// Shaped like the legacy steer transcript entry: committed, not accepted.
		message := head.sessionMessage()
		message.Pending = false
		message.Message.CreatedAt = ""
		var write func(context.Context) error
		if d.r.sessionStore != nil {
			write = func(ctx context.Context) error {
				id, err := d.r.sessionStore.AddMessage(ctx, d.identityID, message)
				if errors.Is(err, session.ErrNotFound) {
					// Unpersisted session: admission treats this as an in-memory-only input.
					return nil
				}
				if err == nil {
					message.ID = id
				}
				return err
			}
		}
		return sessionIOReservation{write: write, commit: func(err error) error {
			if err != nil {
				// Fail closed: the write may have committed, so the head is kept and
				// no automatic wake or retry runs until an explicit retry is admitted.
				d.unidentifiedUncertain = true
				d.lastError = err.Error()
				d.publishPromotionFailureLocked("", err)
				return err
			}
			// An acknowledged row is durable. It becomes visible even when the queue
			// was fenced, but only an unfenced identical head may be consumed.
			pos := d.sess.AddMessageAt(message)
			event := inputEventMetadata(UserMessage(head.Content, d.identityID, head.MultiContent, pos), head).(*UserMessageEvent)
			event.ownerAppend = true
			d.events.Publish(d.identityID, event)
			if fenced() || len(d.pending) == 0 || !sameUnidentifiedInput(d.pending[0], head) {
				d.unidentifiedUncertain = true
				stale := &SessionError{Kind: SessionErrorStale, Operation: "promote_input", Detail: "acknowledged input no longer heads the queue; explicit retry is required"}
				d.lastError = stale.Error()
				d.publishPromotionFailureLocked("", stale)
				return stale
			}
			consumeHead()
			position = pos
			return nil
		}}, nil
	})
	return position, err
}

// promotedRunInput is the ID-less head appended at run start. Its hooks use
// this identity, so the prompt is not re-read from the transcript tail.
type promotedRunInput struct {
	msg      QueuedMessage
	position int
}

type promotedRunInputKey struct{}

func withPromotedRunInput(ctx context.Context, run *promotedRunInput) context.Context {
	return context.WithValue(ctx, promotedRunInputKey{}, run)
}

// initialTurnInputFor returns the run's initial prompt. A promoted ID-less head
// takes precedence; its position is -1 when the run has no promoted head.
func (r *LocalRuntime) initialTurnInputFor(ctx context.Context, sess *session.Session) (QueuedMessage, int, bool) {
	if run, ok := ctx.Value(promotedRunInputKey{}).(*promotedRunInput); ok && run != nil {
		return run.msg, run.position, true
	}
	msg, ok := r.initialTurnInput(sess)
	return msg, -1, ok
}

// unidentifiedStartHead is the ID-less head a start reserved for one generation.
// The entry stays in d.pending, so busy and queued checks still see it, until
// the generation's promotion commits or the generation ends.
type unidentifiedStartHead struct {
	generation uint64
	head       QueuedMessage
}

// clearStartUnidentifiedHeadLocked drops the reservation when its generation
// ends. A reservation for any other generation is left alone.
func (d *sessionDriver) clearStartUnidentifiedHeadLocked(generation uint64) {
	if d.startUnidentifiedHead != nil && d.startUnidentifiedHead.generation == generation {
		d.startUnidentifiedHead = nil
	}
}

// promoteUnidentifiedHead promotes the ID-less head reserved for generation at
// its start into the transcript, before the execution snapshot is cloned. It
// returns the promoted input for the run's hooks, or nil when nothing was
// appended. A generation whose start head had an ID, or was empty, promotes
// nothing. If the reserved head no longer heads the queue, the promotion fails
// as stale and no other head is consumed. skipModel reports a consumed retry
// marker, which carries no content.
func (d *sessionDriver) promoteUnidentifiedHead(ctx context.Context, generation uint64) (run *promotedRunInput, skipModel bool, err error) {
	var head QueuedMessage
	found := false
	if err := d.ownerCall(ctx, func() error {
		if reserved := d.startUnidentifiedHead; reserved != nil && reserved.generation == generation {
			head, found = reserved.head, true
		}
		return nil
	}); err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	position, err := d.promoteUnidentifiedInput(ctx, head)
	if err != nil {
		return nil, false, err
	}
	_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
		if d.generation == generation {
			d.clearStartUnidentifiedHeadLocked(generation)
		}
		return nil
	})
	if head.Retry {
		return nil, true, nil
	}
	return &promotedRunInput{msg: head, position: position}, false, nil
}

// sameUnidentifiedInput identifies a queue entry without a request ID by every
// field of the entry, so distinct inputs with equal text never match.
func sameUnidentifiedInput(a, b QueuedMessage) bool {
	return a.RequestID == "" && b.RequestID == "" && reflect.DeepEqual(a, b)
}

func (d *sessionDriver) drainGuidance(ctx context.Context) []QueuedMessage {
	var batch []QueuedMessage
	_ = d.ownerCall(ctx, func() error {
		if d.compactReserved {
			return nil
		}
		for _, msg := range d.pending {
			if msg.guidance() {
				batch = append(batch, msg)
			}
		}
		slices.SortStableFunc(batch, func(a, b QueuedMessage) int { return a.AcceptedPosition - b.AcceptedPosition })
		return nil
	})
	var drained []QueuedMessage
	consume := func(msg QueuedMessage) {
		d.pending = slices.DeleteFunc(d.pending, func(queued QueuedMessage) bool {
			return queued.RequestID == msg.RequestID && queued.Content == msg.Content && queued.AcceptedPosition == msg.AcceptedPosition
		})
		d.refreshSteeringLocked()
		d.notifyTurnChangedLocked()
	}
	for _, msg := range batch {
		if msg.RequestID == "" {
			if err := d.ownerCall(ctx, func() error { consume(msg); return nil }); err != nil {
				break
			}
			drained = append(drained, msg)
			continue
		}
		completed := make(chan QueuedMessage, 1)
		if err := d.promoteInput(ctx, msg.RequestID, func(value QueuedMessage) {
			consume(value)
			completed <- value
		}); err != nil {
			break
		}
		drained = append(drained, <-completed)
	}
	return drained
}

// A canceled start becomes a settling generation without launching execution.
// Ordinary settlement retries retain its canceled outcome on storage failure.
func (d *sessionDriver) finishCanceledStartFor(startGeneration uint64, startDone chan struct{}) bool {
	var canceled bool
	var generation uint64
	_ = d.ownerCall(d.r.durabilityContext(), func() error {
		if !d.starting() || !d.startCanceled || startDone != nil && (d.generation != startGeneration || d.startDone != startDone) {
			return nil
		}
		canceled = true
		turnID := d.activeRequestID
		d.flushAllDeferredAcceptedLocked()
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
