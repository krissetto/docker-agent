package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// coordination owns child metadata/outbox; transcript rows are written only by
// the session's normal persistence observer and inbox admission.
func (m *subagentManager) coordination() session.CoordinationStore {
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	if m.coord == nil {
		if store, ok := m.r.sessionStore.(session.CoordinationStore); ok {
			m.coord = store
		} else {
			m.coord = session.NewInMemorySessionStore().(session.CoordinationStore)
		}
	}
	return m.coord
}

func (m *subagentManager) admitDurableChild(parent, child *session.Session, record session.ChildRecord) error {
	store := m.coordination()
	if _, native := m.r.sessionStore.(session.CoordinationStore); !native {
		volatile := store.(session.Store)
		_ = volatile.AddSession(m.ctx, parent.OwnSnapshot())
		if parent.ID != record.RootSessionID {
			_ = volatile.AddSession(m.ctx, session.New(session.WithID(record.RootSessionID)))
		}
	}
	return store.AdmitChild(m.ctx, session.ChildAdmission{Child: child, Record: record})
}

func (m *subagentManager) completeSessionTurn(d *sessionDriver, turnID, runErr string) error {
	return m.completeSessionTurnContext(m.r.durabilityContext(), d, turnID, runErr)
}

func (m *subagentManager) completeSessionTurnContext(ctx context.Context, d *sessionDriver, turnID, runErr string) error {
	sess := d.session()
	if sess == nil || !sess.AsyncSubagent {
		return nil
	}
	m.mu.Lock()
	var id subagent.NodeID
	var rec *childRecord
	for nodeID, candidate := range m.children {
		if candidate.sessionID == sess.ID {
			id, rec = nodeID, candidate
			break
		}
	}
	if rec == nil {
		m.mu.Unlock()
		return nil
	}
	record := rec.durable
	if turnID != "" && record.LastTurnID == turnID {
		m.mu.Unlock()
		return nil
	}
	state := subagent.NodeIdle
	if runErr != "" {
		state = subagent.NodeFailed
	}
	if rec.state == subagent.NodeStopped {
		state = subagent.NodeStopped
	}
	d.mu.Lock()
	result := d.generationResult
	d.mu.Unlock()
	preview, truncated := subagent.PreviewText(result, subagent.PreviewLen)
	record.Node.State, record.Node.Error = state, runErr
	record.Node.NeedsAttention = state == subagent.NodeFailed
	record.Result, record.Error, record.LastTurnID = preview, runErr, turnID
	parentID, parentAgent := rec.parentSession, rec.parentAgentName
	report := session.ChildReport{}
	if turnID != "" && state != subagent.NodeStopped && !m.hasRunningSubagentsLocked(sess.ID) {
		detail := preview
		if truncated {
			detail += " [...]"
		}
		if runErr != "" {
			detail = runErr
		}
		report = session.ChildReport{ID: childReportID(sess.ID, turnID), ParentSessionID: parentID, ChildSessionID: sess.ID, TurnID: turnID, Content: childTurnReport(rec.name, id, state, detail, truncated)}
	}
	m.mu.Unlock()
	if record.Revision != 0 {
		commit := session.ChildCommit{ExpectedRevision: record.Revision, Record: record}
		if report.ID != "" {
			commit.Reports = []session.ChildReport{report}
		}
		ctx, cancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
		defer cancel()
		err := m.coordination().CommitChild(ctx, commit)
		if err == nil {
			record.Revision++
		}
		if errors.Is(err, session.ErrRevisionConflict) {
			records, loadErr := m.coordination().LoadChildren(ctx, record.RootSessionID)
			if loadErr != nil {
				return loadErr
			}
			for _, persisted := range records {
				if persisted.Node.ID == id && persisted.LastTurnID == turnID && persisted.Revision >= record.Revision+1 {
					record = persisted
					err = nil
					break
				}
			}
		}
		if err != nil {
			return err
		}
	}
	if parent, ok := m.r.sessionDrivers.Lookup(parentID); ok && m.r.team != nil {
		a, _ := m.r.team.Agent(parentAgent)
		m.r.executeSubagentStopHooks(m.r.lifetime(), parent.session(), sess, a, sess.AgentName, result)
	}
	m.mu.Lock()
	if current := m.children[id]; current == rec {
		current.durable = record
		current.result, current.errMsg, current.state = preview, runErr, state
		_ = m.tree.Update(id, func(n *subagent.Node) {
			n.State, n.Error, n.NeedsAttention = state, runErr, state == subagent.NodeFailed
		})
	}
	m.mu.Unlock()
	m.persistSnapshot()
	m.r.sessionDrivers.signalWork()
	return nil
}

func (g *sessionDriverRegistry) deliverReports(d *sessionDriver) bool {
	if g.r.subagents == nil {
		return false
	}
	d.mu.Lock()
	if d.viewDormant {
		d.mu.Unlock()
		return false
	}
	retry := d.reportRetry
	d.mu.Unlock()
	if retry != nil {
		if err := d.acceptReport(g.r.lifetime(), *retry); err != nil {
			return true
		}
		d.mu.Lock()
		d.reportRetry = nil
		d.mu.Unlock()
	}
	reports, err := g.r.subagents.coordination().PendingReports(g.r.lifetime(), d.sessionID())
	if err != nil {
		return true
	}
	for _, report := range reports {
		if err := d.acceptReport(g.r.lifetime(), report); err != nil {
			d.mu.Lock()
			d.reportRetry = &report
			d.mu.Unlock()
			return true
		}
	}
	return false
}

func childReportID(childSessionID, turnID string) string {
	return "turn:" + base64.RawURLEncoding.EncodeToString([]byte(childSessionID)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(turnID))
}

func (d *sessionDriver) acceptReport(ctx context.Context, report session.ChildReport) error {
	store := d.r.subagents.coordination()
	senderName := ""
	d.r.subagents.mu.Lock()
	for _, child := range d.r.subagents.children {
		if child.sessionID == report.ChildSessionID {
			senderName = child.name
			break
		}
	}
	d.r.subagents.mu.Unlock()
	d.mu.Lock()
	if d.viewDormant {
		d.mu.Unlock()
		return nil
	}
	acceptance, err := store.AcceptReport(ctx, d.sessionIDLocked(), report.ID, nil)
	if err != nil {
		d.mu.Unlock()
		return err
	}
	if acceptance.MessageID == 0 {
		if d.stopped || !limitAllows(len(d.pending), d.pendingLimit()) {
			d.mu.Unlock()
			return ErrSessionCapacity
		}
		message := session.UserMessage(report.Content)
		message.Pending, message.Accepted, message.TurnID = true, true, "report:"+report.ID
		message.InputOrigin, message.InputMode = session.InputOriginRuntime, "steer"
		message.SenderID, message.SenderName = report.ChildSessionID, senderName
		acceptance, err = store.AcceptReport(ctx, d.sessionIDLocked(), report.ID, message)
		if err != nil {
			d.mu.Unlock()
			return err
		}
	}
	message := acceptance.Message
	if message == nil || !message.Pending || !message.Accepted {
		d.mu.Unlock()
		return nil
	}
	position := -1
	for i, item := range d.sess.MessagesSnapshot() {
		if item.Message != nil && item.Message.TurnID == message.TurnID {
			if !item.Message.Pending {
				d.mu.Unlock()
				return nil
			}
			position = i
			break
		}
	}
	for _, queued := range append(slices.Clone(d.pending), d.steering...) {
		if queued.RequestID == message.TurnID {
			d.mu.Unlock()
			return nil
		}
	}
	if position < 0 {
		position = d.sess.AddMessageAt(message)
	}
	_, persisted := d.r.sessionStore.(session.CoordinationStore)
	msg := queuedSessionInput(message, position, persisted)
	d.pending = append(d.pending, msg)
	d.refreshSteeringLocked()
	d.events.PublishForRequest(d.sessionIDLocked(), msg.RequestID, inputEventMetadata(PendingUserMessageAccepted(d.sessionIDLocked(), msg.RequestID, msg.Content, msg.MultiContent, position), msg))
	d.mu.Unlock()
	d.WakePending()
	return nil
}

func childTurnReport(name string, id subagent.NodeID, state subagent.NodeState, detail string, truncated bool) string {
	verb, label := "finished its turn", "Full response"
	if state == subagent.NodeFailed {
		verb, label = "failed", "Error"
	}
	msg := fmt.Sprintf("Subagent %q (%s) %s.", name, id, verb)
	if detail != "" {
		if truncated {
			label += " preview"
		}
		msg += fmt.Sprintf(" %s: %q", label, detail)
	}
	return msg
}

// sessionDurability describes the complete admission/output/report contract,
// not merely whether a legacy topology snapshot can be saved.
func (r *LocalRuntime) sessionDurability() subagent.Durability {
	if _, ok := r.sessionStore.(session.CoordinationStore); !ok {
		return subagent.DurabilityVolatile
	}
	if _, ok := r.sessionStore.(session.ItemAppender); !ok {
		return subagent.DurabilityVolatile
	}
	store, ok := r.sessionStore.(interface{ Durability() subagent.Durability })
	if !ok || store.Durability() != subagent.DurabilityDurable {
		return subagent.DurabilityVolatile
	}
	return subagent.DurabilityDurable
}

func ownAssistantResult(sess *session.Session) string {
	messages := sess.OwnMessages()
	for _, message := range slices.Backward(messages) {
		if message.Message.Role == chat.MessageRoleAssistant {
			return message.Message.Content
		}
	}
	return ""
}
