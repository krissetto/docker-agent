package runtime

import (
	"context"
	"reflect"
	"strings"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// Only the manual Submit boundary can consume a stopped-view capability.
func (h *sessionHandle) reactivateStoppedView(ctx context.Context, msg QueuedMessage) (bool, error) {
	d := h.driver
	if !d.registrySnapshot().stoppedView {
		return false, nil
	}
	m, g := h.runtime.subagents, h.runtime.sessionDrivers
	transition := m.transition(m.rootSessionLockedSafe(h.sessionID))
	transition.Lock()
	defer transition.Unlock()
	if !d.registrySnapshot().stoppedView {
		return false, nil
	}
	stopped := &SessionError{Kind: SessionErrorStopped, SessionID: h.sessionID, Operation: "submit"}
	store, ok := h.runtime.sessionStore.(session.ChildReactivationStore)
	if !ok {
		return true, UnsupportedSessionOperation(h.sessionID, "reactivate_child")
	}
	m.mu.Lock()
	state := m.sessions[h.sessionID]
	var rec *childRecord
	if state != nil {
		rec = m.children[state.node]
	}
	if m.closed || rec == nil || rec.agent == nil || rec.durable.Node.State != subagent.NodeStopped {
		m.mu.Unlock()
		return true, stopped
	}
	expected := rec.durable
	m.mu.Unlock()
	g.mu.Lock()
	_, deleted := g.deleted[h.sessionID]
	valid := g.drivers[h.sessionID] == d && !g.closed && !deleted
	g.mu.Unlock()
	if !valid || msg.Retry || strings.TrimSpace(msg.Content) == "" && len(msg.MultiContent) == 0 {
		return true, stopped
	}
	input := msg.sessionMessage()
	input.TurnID, input.Pending, input.Accepted = msg.RequestID, true, true
	var record session.ChildRecord
	var accepted *session.Message
	err := d.durableIO(ctx, func() (sessionIOReservation, error) {
		if !d.stopped || !d.stoppedView || d.reclaiming || d.editReserved {
			return sessionIOReservation{}, stopped
		}
		if found, _, err := d.existingInputLocked(msg); found || err != nil {
			if err != nil {
				return sessionIOReservation{}, err
			}
			return sessionIOReservation{}, stopped
		}
		if !limitAllows(0, d.pendingLimit()) {
			return sessionIOReservation{}, &SessionError{Kind: SessionErrorCapacity, SessionID: h.sessionID, Operation: "submit", Limit: d.pendingLimit()}
		}
		d.editReserved = true
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				var err error
				record, accepted, err = store.ReactivateChildWithInput(ctx, expected, input)
				if err == nil {
					return nil
				}
				reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultSubagentPersistenceTimeout)
				defer cancel()
				records, loadErr := m.coordination().LoadChildren(reconcileCtx, expected.RootSessionID)
				if loadErr != nil {
					return err
				}
				wanted := expected
				wanted.Revision++
				wanted.Node.State, wanted.Node.NeedsAttention, wanted.Node.WaitingOn = subagent.NodeIdle, false, ""
				wanted.ReactivationTurnID = msg.RequestID
				matched := false
				for _, candidate := range records {
					if reflect.DeepEqual(candidate, wanted) {
						record, matched = candidate, true
						break
					}
				}
				if !matched {
					return err
				}
				stored, loadErr := h.runtime.sessionStore.GetSession(reconcileCtx, h.sessionID)
				if loadErr != nil {
					return err
				}
				for _, item := range stored.MessagesSnapshot() {
					if item.Message == nil || item.Message.TurnID != msg.RequestID {
						continue
					}
					receipt := *item.Message
					receipt.ID = input.ID
					if reflect.DeepEqual(receipt, *input) {
						accepted = item.Message
						return nil
					}
				}
				return err
			},
			commit: func(err error) error {
				d.editReserved = false
				if err != nil {
					return err
				}
				owned := d.sess.Clone()
				for _, item := range owned.Messages {
					if item.Message != nil && item.Message.Pending {
						item.Message.Accepted = false
					}
				}
				position := owned.AddMessageAt(accepted)
				d.sess = owned
				d.pending = []QueuedMessage{queuedSessionInput(accepted, position, true)}
				d.steering = nil
				d.stopped, d.stoppedView = false, false
				d.authorizeViewLocked()
				d.events.PublishForRequest(h.sessionID, msg.RequestID, inputEventMetadata(PendingUserMessageAccepted(h.sessionID, msg.RequestID, msg.Content, msg.MultiContent, position), msg))
				return nil
			},
		}, nil
	})
	if err != nil {
		return true, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec.durable = record
	_ = m.tree.Update(record.Node.ID, func(n *subagent.Node) { *n = record.Node })
	return true, nil
}
