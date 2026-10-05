package runtime

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/session"

	"github.com/docker/docker-agent/pkg/subagent"
)

func (h *sessionHandle) stopRootTree(ctx context.Context) error {
	m, g := h.runtime.subagents, h.runtime.sessionDrivers
	m.mu.Lock()
	g.mu.Lock()
	if g.drivers[h.sessionID] != h.driver {
		g.mu.Unlock()
		m.mu.Unlock()
		return &SessionError{Kind: SessionErrorStale, SessionID: h.sessionID, Operation: "stop_subtree"}
	}
	if g.stoppedTrees == nil {
		g.stoppedTrees = make(map[string]struct{})
	}
	g.stoppedTrees[h.sessionID] = struct{}{}
	var children []subagent.NodeID
	for id, rec := range m.children {
		if rec.parentSession == h.sessionID {
			children = append(children, id)
		}
	}
	g.mu.Unlock()
	m.mu.Unlock()
	if err := h.driver.ownerCall(context.WithoutCancel(ctx), func() error { h.driver.durableStopRequested = true; return nil }); err != nil {
		return err
	}
	h.driver.StopAll()
	var result error
	result = errors.Join(result, h.driver.withdrawStoppedInputs(ctx))
	for _, id := range children {
		_, err := m.stopChildContext(ctx, h.sessionID, id)
		result = errors.Join(result, err)
	}
	select {
	case <-h.driver.Done():
	case <-ctx.Done():
		return errors.Join(result, ctx.Err())
	}
	var settling bool
	var generation uint64
	var runErr string
	err := h.driver.ownerCall(ctx, func() error {
		settling, generation, runErr = h.driver.settling(), h.driver.generation, h.driver.completionRunErr
		return nil
	})
	if err != nil {
		return errors.Join(result, err)
	}
	if settling {
		h.driver.finishRunContext(ctx, generation, runErr)
	}
	err = h.driver.ownerCall(ctx, func() error { return h.driver.completionErr })
	return errors.Join(result, err)
}

// Explicit root stop withdraws durable pending input; shutdown merely detaches
// ownership and preserves queued input for a later runtime.
func (d *sessionDriver) withdrawStoppedInputs(ctx context.Context) error {
	return d.durableIO(ctx, d.reserveStoppedWithdrawal)
}

func (d *sessionDriver) reserveStoppedWithdrawal() (sessionIOReservation, error) {
	next := d.sess.OwnSnapshot()
	var turnIDs []string
	for _, item := range next.Messages {
		if item.Message != nil && item.Message.Pending && item.Message.Accepted {
			turnIDs = append(turnIDs, item.Message.TurnID)
		}
	}
	var write func(context.Context) error
	if d.r.sessionStore != nil {
		deleter, ok := d.r.sessionStore.(session.PendingMessageDeleter)
		if !ok && len(turnIDs) != 0 {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorUnsupported, Operation: "stop_subtree", Detail: "store cannot withdraw durable pending input"}
		}
		write = func(ctx context.Context) error {
			for _, id := range turnIDs {
				if err := deleter.DeletePendingUserMessage(ctx, next.ID, id); err != nil && !errors.Is(err, session.ErrNotFound) {
					return err
				}
				next.RemovePendingUserMessageByTurnID(id)
				next.SetTurnOutcome(id, string(TurnCanceled))
			}
			return d.r.sessionStore.UpdateSession(ctx, next)
		}
	}
	return sessionIOReservation{write: write, commit: func(err error) error {
		if err != nil {
			d.completionErr = err
			return err
		}
		for _, id := range turnIDs {
			d.sess.RemovePendingUserMessageByTurnID(id)
			d.sess.SetTurnOutcome(id, string(TurnCanceled))
			d.completeTurnLocked(id)
		}
		d.pending, d.steering = nil, nil
		d.durableStopRequested = false
		d.refreshSteeringLocked()
		return nil
	}}, nil
}
