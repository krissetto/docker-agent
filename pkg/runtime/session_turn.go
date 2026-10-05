package runtime

import (
	"context"
	"slices"
)

func (r *LocalRuntime) lifetime() context.Context {
	if r.lifecycleCtx != nil {
		return r.lifecycleCtx
	}
	if r.ctx != nil {
		return r.ctx()
	}
	return context.Background() //rubocop:disable Lint/ContextConnectivity // isolated test drivers have no supervisor
}

const SessionErrorInterrupted SessionErrorKind = "interrupted"

// persistTurnOutcomeLocked commits terminal evidence before publishing settlement.
// A promoted admission without this evidence is uncertain after a restart.
func (d *sessionDriver) persistTurnOutcome(ctx context.Context, id string, outcome TurnOutcome) error {
	return d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.sess == nil {
			return sessionIOReservation{}, nil
		}
		next := d.sess.OwnSnapshot()
		consumed := slices.Clone(d.consumedSteering)
		next.SetTurnOutcome(id, string(outcome))
		for _, steeringID := range consumed {
			next.SetTurnOutcome(steeringID, string(outcome))
		}
		var write func(context.Context) error
		if d.r.sessionStore != nil {
			write = func(ctx context.Context) error { return d.r.sessionStore.UpdateSession(ctx, next) }
		}
		return sessionIOReservation{write: write, commit: func(err error) error {
			if err != nil {
				return err
			}
			d.sess.SetTurnOutcome(id, string(outcome))
			for _, steeringID := range consumed {
				d.sess.SetTurnOutcome(steeringID, string(outcome))
			}
			d.consumedSteering = slices.DeleteFunc(d.consumedSteering, func(value string) bool { return slices.Contains(consumed, value) })
			return nil
		}}, nil
	})
}

type InteractionResolution string

const (
	InteractionResponded InteractionResolution = "responded"
	InteractionCanceled  InteractionResolution = "canceled"
	InteractionStopped   InteractionResolution = "stopped"
)

type InteractionResolvedEvent struct {
	AgentContext

	Type          string                `json:"type"`
	SessionID     string                `json:"session_id"`
	InteractionID string                `json:"interaction_id"`
	Reason        InteractionResolution `json:"reason"`
}

func (e *InteractionResolvedEvent) GetSessionID() string { return e.SessionID }

func (d *sessionDriver) resolveInteractionLocked(id string) {
	if _, exists := d.interactions[id]; !exists {
		return
	}
	delete(d.interactions, id)
	d.events.Publish(d.sessionIDLocked(), &InteractionResolvedEvent{Type: "interaction_resolved", SessionID: d.sessionIDLocked(), InteractionID: id, Reason: InteractionResponded})
}

func (d *sessionDriver) resolveInteractionsLocked() {
	reason := InteractionCanceled
	if d.stopped {
		reason = InteractionStopped
	}
	for id, interaction := range d.interactions {
		if interaction.waiter != nil {
			interaction.waiter.tryCancel()
		}
		delete(d.interactions, id)
		d.events.Publish(d.sessionIDLocked(), &InteractionResolvedEvent{Type: "interaction_resolved", SessionID: d.sessionIDLocked(), InteractionID: id, Reason: reason})
	}
}

func (d *sessionDriver) notifyTurnChangedLocked() {
	if d.turnChanged != nil {
		close(d.turnChanged)
	}
	d.turnChanged = make(chan struct{})
}

func (d *sessionDriver) completeTurnLocked(id string) {
	if id != "" {
		d.completedTurns = append(d.completedTurns, id)
		// Withdrawal and retry have no transcript identity. Keep a bounded recent window.
		if len(d.completedTurns) > defaultMaxSubagentMailbox {
			d.completedTurns = d.completedTurns[1:]
		}
	}
	d.notifyTurnChangedLocked()
}

// AwaitTurn waits for this accepted turn, not for session-wide idleness. Unknown
// or expired withdrawn/retry identities return not_found, never inferred success.
func (h *sessionHandle) AwaitTurn(ctx context.Context, turnID string) error {
	d := h.driver
	known := false
	for {
		d.mu.Lock()
		active := d.activeRequestID == turnID && (d.running() || d.starting() || d.settling())
		consumed := slices.Contains(d.consumedSteering, turnID)
		pending := active
		for _, msg := range append(slices.Clone(d.pending), d.steering...) {
			pending = pending || msg.RequestID == turnID
		}
		completed := slices.Contains(d.completedTurns, turnID) || consumed
		terminal := ""
		if d.sess != nil {
			terminal = d.sess.TurnOutcome(turnID)
		}
		known = known || pending || completed || terminal != ""
		if !known && d.sess != nil {
			for _, item := range d.sess.MessagesSnapshot() {
				if item.Message != nil && item.Message.TurnID == turnID && item.Message.Accepted {
					known = true
					break
				}
			}
		}
		changed := d.turnChanged
		stopped := d.stopped
		completionErr := d.completionErr
		d.mu.Unlock()
		if turnID == "" || !known {
			return &SessionError{Kind: SessionErrorNotFound, SessionID: h.sessionID, RequestID: turnID, Operation: "await_turn"}
		}
		if completionErr != nil {
			return completionErr
		}
		if !pending || (stopped && !active) {
			if completed || terminal == string(TurnCompleted) || terminal == string(TurnFailed) || terminal == string(TurnCanceled) {
				return nil
			}
			return &SessionError{Kind: SessionErrorInterrupted, SessionID: h.sessionID, RequestID: turnID, Operation: "await_turn", Detail: "turn has no durable terminal outcome; execution may have been interrupted and must not be automatically replayed"}
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *LocalRuntime) durabilityContext() context.Context {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	if r.drainCtx != nil {
		return r.drainCtx
	}
	return r.lifetime()
}

// interruptedTurnsLocked separates execution uncertainty from scheduler idleness.
func (d *sessionDriver) interruptedTurnsLocked() int {
	if d.sess == nil {
		return 0
	}
	outcomes := d.sess.TurnOutcomesSnapshot()
	count := 0
	for _, item := range d.sess.MessagesSnapshot() {
		msg := item.Message
		if msg == nil || !msg.Accepted || msg.Pending || msg.TurnID == "" {
			continue
		}
		if msg.TurnID == d.activeRequestID || slices.Contains(d.consumedSteering, msg.TurnID) {
			continue
		}
		outcome := outcomes[msg.TurnID]
		if outcome != string(TurnCompleted) && outcome != string(TurnFailed) && outcome != string(TurnCanceled) {
			count++
		}
	}
	return count
}
