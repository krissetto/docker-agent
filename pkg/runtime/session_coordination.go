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
	err := store.AdmitChild(m.ctx, session.ChildAdmission{Child: child, Record: record})
	if err == nil {
		return nil
	}
	records, loadErr := store.LoadChildren(m.ctx, record.RootSessionID)
	if loadErr != nil {
		return err
	}
	for _, persisted := range records {
		if persisted.Node.ID == record.Node.ID && persisted.Node.SessionID == child.ID {
			return nil
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.admissions, child.ID)
	return err
}

func (m *subagentManager) completeSessionTurn(d *sessionDriver, turnID, runErr string) error {
	return m.completeSessionTurnContext(m.r.durabilityContext(), d, turnID, runErr)
}

func (m *subagentManager) completeSessionTurnContext(ctx context.Context, d *sessionDriver, turnID, runErr string) error {
	sess := d.session()
	if sess == nil || !sess.AsyncSubagent {
		return nil
	}
	transition := m.transition(m.rootSessionLockedSafe(d.sessionID()))
	transition.Lock()
	transitionHeld := true
	defer func() {
		if transitionHeld {
			transition.Unlock()
		}
	}()
	m.mu.Lock()
	var id subagent.NodeID
	var rec *childRecord
	for nodeID, candidate := range m.children {
		if candidate.sessionID == sess.ID {
			id, rec = nodeID, candidate
			break
		}
	}
	if rec == nil || rec.durable.Node.State == subagent.NodeStopped {
		m.mu.Unlock()
		return nil
	}
	if m.sessionStoppingLocked(sess.ID) {
		m.mu.Unlock()
		return &SessionError{Kind: SessionErrorPersistence, SessionID: sess.ID, Operation: "complete_turn", Detail: "subtree stop is awaiting authoritative acknowledgement"}
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

	d.mu.Lock()
	result := d.generationResult
	d.mu.Unlock()
	preview, truncated := subagent.PreviewText(result, subagent.PreviewLen)
	record.Node.State, record.Node.Error = state, runErr
	record.Node.NeedsAttention = state == subagent.NodeFailed
	record.Result, record.LastTurnID = preview, turnID
	parentID, parentAgent := rec.parentSession, rec.parentAgentName
	report := session.ChildReport{}
	if !rec.synchronous && turnID != "" && state != subagent.NodeStopped && !m.hasRunningSubagentsLocked(sess.ID) {
		detail := preview
		if truncated {
			detail += " [...]"
		}
		if runErr != "" {
			detail = runErr
		}
		report = session.ChildReport{ID: childReportID(sess.ID, turnID), ParentSessionID: parentID, ChildSessionID: sess.ID, TurnID: turnID, Content: childTurnReport(rec.name, id, state, detail, truncated), ReportOutcome: childReportOutcome(state)}
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
			reconcileCtx, reconcileCancel := context.WithTimeout(context.WithoutCancel(ctx), defaultSubagentPersistenceTimeout)
			defer reconcileCancel()
			records, loadErr := m.coordination().LoadChildren(reconcileCtx, record.RootSessionID)
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
	m.mu.Lock()
	if current := m.children[id]; current == rec {
		current.durable = record
		_ = m.tree.Update(id, func(n *subagent.Node) {
			n.State, n.Error, n.NeedsAttention = state, runErr, state == subagent.NodeFailed
		})
	}
	m.mu.Unlock()
	transition.Unlock()
	transitionHeld = false
	parentSession := rec.parentSess
	if parent, ok := m.r.sessionDrivers.Lookup(parentID); ok {
		parentSession = parent.session()
	}
	if parentSession != nil && m.r.team != nil {
		a, _ := m.r.team.Agent(parentAgent)
		m.r.executeSubagentStopHooks(m.r.lifetime(), parentSession, sess, a, sess.AgentName, result)
	}
	m.mu.Lock()
	if current := m.children[id]; current == rec {
		current.parentSess = nil
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
	var retry *session.ChildReport
	dormant := false
	_ = d.ownerCall(g.r.lifetime(), func() error { dormant, retry = d.viewDormant, d.reportRetry; return nil })
	if dormant {
		return false
	}
	if retry != nil {
		if err := d.acceptReport(g.r.lifetime(), *retry); err != nil {
			return true
		}
		_ = d.ownerCall(g.r.lifetime(), func() error { d.reportRetry = nil; return nil })
	}
	reports, err := g.r.subagents.coordination().PendingReports(g.r.lifetime(), d.sessionID())
	if err != nil {
		return true
	}
	for _, report := range reports {
		if err := d.acceptReport(g.r.lifetime(), report); err != nil {
			_ = d.ownerCall(g.r.lifetime(), func() error { reportSnapshot := report; d.reportRetry = &reportSnapshot; return nil })
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
	err := d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.viewDormant {
			return sessionIOReservation{}, nil
		}
		message := session.UserMessage(report.Content)
		message.Pending, message.Accepted, message.TurnID = true, true, "report:"+report.ID
		message.InputOrigin, message.InputMode = session.InputOriginRuntime, "steer"
		message.SenderID, message.SenderName = report.ChildSessionID, senderName
		message.ReportOutcome = report.ReportOutcome
		canAdmit := !d.stopped && limitAllows(len(d.pending), d.pendingLimit())
		var acceptance session.ReportAcceptance
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				var err error
				acceptance, err = store.AcceptReport(ctx, d.identityID, report.ID, nil)
				if err != nil {
					return err
				}
				if acceptance.MessageID == 0 {
					if !canAdmit {
						return ErrSessionCapacity
					}
					acceptance, err = store.AcceptReport(ctx, d.identityID, report.ID, message)
				}
				return err
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				message := acceptance.Message
				if message == nil || !message.Pending || !message.Accepted {
					return nil
				}
				position := -1
				for i, item := range d.sess.MessagesSnapshot() {
					if item.Message != nil && item.Message.TurnID == message.TurnID {
						if !item.Message.Pending {
							return nil
						}
						position = i
						break
					}
				}
				for _, queued := range append(slices.Clone(d.pending), d.steering...) {
					if queued.RequestID == message.TurnID {
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
				d.events.PublishForRequest(d.identityID, msg.RequestID, inputEventMetadata(PendingUserMessageAccepted(d.identityID, msg.RequestID, msg.Content, msg.MultiContent, position), msg))
				return nil
			},
		}, nil
	})
	if err == nil {
		d.WakePending()
	}
	return err
}

func childReportOutcome(state subagent.NodeState) session.ReportOutcome {
	if state == subagent.NodeFailed {
		return session.ReportOutcomeFailed
	}
	return session.ReportOutcomeFinished
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
	var fallback string
	foundFallback := false
	for _, message := range slices.Backward(messages) {
		if message.Message.Role != chat.MessageRoleAssistant {
			continue
		}
		if len(message.Message.ToolCalls) == 0 {
			return message.Message.Content
		}
		if !foundFallback {
			fallback = message.Message.Content
			foundFallback = true
		}
	}
	return fallback
}
