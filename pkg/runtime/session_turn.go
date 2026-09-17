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
	for id := range d.interactions {
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
		pending := active
		for _, msg := range append(slices.Clone(d.pending), d.steering...) {
			pending = pending || msg.RequestID == turnID
		}
		known = known || pending || slices.Contains(d.completedTurns, turnID)
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
			return nil
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
