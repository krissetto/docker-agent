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
	d.mu.Lock()
	stoppedView := d.stoppedView
	d.mu.Unlock()
	if !stoppedView {
		return false, nil
	}
	m, g := h.runtime.subagents, h.runtime.sessionDrivers
	g.runMu.Lock()
	defer g.runMu.Unlock()
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.stopped {
		return false, nil
	}
	stopped := func() (bool, error) {
		return true, &SessionError{Kind: SessionErrorStopped, SessionID: h.sessionID, Operation: "submit"}
	}
	if !d.stoppedView || g.drivers[h.sessionID] != d || g.closed || m.closed || h.runtime.lifetime().Err() != nil || d.reclaiming || msg.Retry || (strings.TrimSpace(msg.Content) == "" && len(msg.MultiContent) == 0) {
		return stopped()
	}
	if _, deleted := g.deleted[h.sessionID]; deleted {
		return stopped()
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if found, _, err := d.existingInputLocked(msg); found || err != nil {
		if err != nil {
			return true, err
		}
		return stopped()
	}
	store, ok := h.runtime.sessionStore.(session.ChildReactivationStore)
	if !ok {
		return true, UnsupportedSessionOperation(h.sessionID, "reactivate_child")
	}
	state := m.sessions[h.sessionID]
	if state == nil {
		return stopped()
	}
	rec := m.children[state.node]
	if rec == nil || rec.agent == nil || rec.durable.Node.State != subagent.NodeStopped {
		return stopped()
	}
	if !limitAllows(0, d.pendingLimit()) {
		return true, &SessionError{Kind: SessionErrorCapacity, SessionID: h.sessionID, Operation: "submit", Limit: d.pendingLimit()}
	}
	input := msg.sessionMessage()
	input.TurnID, input.Pending, input.Accepted = msg.RequestID, true, true
	expected := rec.durable
	record, accepted, err := store.ReactivateChildWithInput(ctx, expected, input)
	if err != nil {
		// A lost acknowledgement is success only with the exact revision and receipt.
		records, loadErr := m.coordination().LoadChildren(context.WithoutCancel(ctx), expected.RootSessionID)
		if loadErr != nil {
			return true, err
		}
		matched := false
		wanted := expected
		wanted.Revision++
		wanted.Node.State, wanted.Node.NeedsAttention, wanted.Node.WaitingOn = subagent.NodeIdle, false, ""
		wanted.ReactivationTurnID = msg.RequestID
		for _, candidate := range records {
			if reflect.DeepEqual(candidate, wanted) {
				record = candidate
				matched = true
				break
			}
		}
		if !matched {
			return true, err
		}
		stored, loadErr := h.runtime.sessionStore.GetSession(context.WithoutCancel(ctx), h.sessionID)
		if loadErr != nil {
			return true, err
		}
		for _, item := range stored.MessagesSnapshot() {
			if item.Message == nil || item.Message.TurnID != msg.RequestID {
				continue
			}
			receipt := *item.Message
			receipt.ID = input.ID
			if reflect.DeepEqual(receipt, *input) {
				accepted = item.Message
				break
			}
		}
		if accepted == nil {
			return true, err
		}
	}
	// Keep the observer driver and its generation, replacing only its transcript
	// snapshot; retired rows remain visible but cannot rehydrate or reach a model.
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
	delete(g.orphans, h.sessionID)
	rec.durable = record
	_ = m.tree.Update(record.Node.ID, func(n *subagent.Node) { *n = record.Node })
	d.stopped, d.stoppedView = false, false
	d.authorizeViewLocked()
	d.events.PublishForRequest(h.sessionID, msg.RequestID, inputEventMetadata(PendingUserMessageAccepted(h.sessionID, msg.RequestID, msg.Content, msg.MultiContent, position), msg))
	return true, nil
}
