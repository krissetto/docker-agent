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
	ctx, cancel := context.WithTimeout(m.r.durabilityContext(), defaultSubagentPersistenceTimeout)
	defer cancel()
	if _, native := m.r.sessionStore.(session.CoordinationStore); !native {
		volatile := store.(session.Store)
		_ = volatile.AddSession(ctx, parent.OwnSnapshot())
		if parent.ID != record.RootSessionID {
			_ = volatile.AddSession(ctx, session.New(session.WithID(record.RootSessionID)))
		}
	}
	err := store.AdmitChild(ctx, session.ChildAdmission{Child: child, Record: record})
	if err == nil {
		return nil
	}
	// A lost acknowledgement must not abandon the reserved admission identity.
	reconcileCtx, reconcileCancel := context.WithTimeout(m.r.durabilityContext(), defaultSubagentPersistenceTimeout)
	defer reconcileCancel()
	records, loadErr := store.LoadChildren(reconcileCtx, record.RootSessionID)
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
	return m.completeSessionTurnContext(m.r.durabilityContext(), d, turnID, runErr, false, false)
}

func (m *subagentManager) completeExecutedSessionTurnContext(ctx context.Context, d *sessionDriver, turnID, runErr string) error {
	return m.completeSessionTurnContext(ctx, d, turnID, runErr, true, false)
}

func (m *subagentManager) completeSessionTurnContext(ctx context.Context, d *sessionDriver, turnID, runErr string, causalWake, deferredReplay bool) error {
	sess := d.session()
	if sess == nil || !sess.AsyncSubagent {
		if deferredReplay {
			d.clearDeferredCompletionReport(context.WithoutCancel(ctx), turnID)
		}
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
		if deferredReplay {
			d.clearDeferredCompletionReport(context.WithoutCancel(ctx), turnID)
		}
		return nil
	}
	if m.sessionStoppingLocked(sess.ID) {
		if deferredReplay {
			_ = d.ownerCall(context.WithoutCancel(ctx), func() error { d.deferredCompletionReportTurnID = ""; return nil })
		}
		m.mu.Unlock()
		return &SessionError{Kind: SessionErrorPersistence, SessionID: sess.ID, Operation: "complete_turn", Detail: "subtree stop is awaiting authoritative acknowledgement"}
	}
	record := rec.durable
	if deferredReplay && record.LastTurnID != turnID {
		m.mu.Unlock()
		d.clearDeferredCompletionReport(context.WithoutCancel(ctx), turnID)
		return nil
	}
	if turnID != "" && record.LastTurnID == turnID && !deferredReplay {
		m.mu.Unlock()
		return nil
	}
	if deferredReplay {
		d.mu.Lock()
		intentMatches := d.deferredCompletionReportTurnID == turnID
		active := d.running() || d.starting() || d.settling() || len(d.pending) != 0 || d.backgroundWorks != 0
		terminal := d.stopped || d.explicitStopLocked() || d.canceledOutcome
		d.mu.Unlock()
		stale := record.LastTurnID != turnID
		subtreeWork := m.hasRunningSubagentsLocked(sess.ID)
		m.mu.Unlock()
		if terminal || stale {
			d.clearDeferredCompletionReport(context.WithoutCancel(ctx), turnID)
			return nil
		}
		if !intentMatches || active || subtreeWork {
			return nil
		}
		m.mu.Lock()
		rec = m.children[id]
		if rec == nil || rec.durable.Node.State == subagent.NodeStopped || rec.durable.LastTurnID != turnID {
			m.mu.Unlock()
			d.clearDeferredCompletionReport(context.WithoutCancel(ctx), turnID)
			return nil
		}
		record = rec.durable
		runErr = record.Node.Error
	}
	state := subagent.NodeIdle
	if deferredReplay {
		state = record.Node.State
	} else if runErr != "" {
		state = subagent.NodeFailed
	}

	d.mu.Lock()
	result, backgroundWork := d.generationResult, d.backgroundWorks != 0
	d.mu.Unlock()
	result = chooseCompletionResult(result, record.Result, deferredReplay)
	preview, truncated := subagent.PreviewText(result, subagent.PreviewLen)
	record.Node.State, record.Node.Error = state, runErr
	record.Node.NeedsAttention = state == subagent.NodeFailed
	record.Result, record.LastTurnID = preview, turnID
	parentID, parentAgent := rec.parentSession, rec.parentAgentName
	report := session.ChildReport{}
	subtreeWork := m.hasRunningSubagentsLocked(sess.ID)
	canReport := !rec.synchronous && turnID != "" && state != subagent.NodeStopped && !subtreeWork
	if !rec.synchronous && turnID != "" && state != subagent.NodeStopped && (backgroundWork || subtreeWork) {
		_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
			d.deferredCompletionReportTurnID = turnID
			return nil
		})
	}
	if canReport && !backgroundWork {
		detail := preview
		if truncated {
			detail += " [...]"
		}
		if runErr != "" {
			detail = runErr
		}
		report = session.ChildReport{ID: childReportID(sess.ID, turnID), ParentSessionID: parentID, ChildSessionID: sess.ID, TurnID: turnID, Revision: record.Revision + 1, Content: childTurnReport(rec.name, id, state, detail, truncated), ReportOutcome: childReportOutcome(state)}
	}
	m.mu.Unlock()
	if deferredReplay {
		d.mu.Lock()
		stillValid := d.deferredCompletionReportTurnID == turnID && !d.running() && !d.starting() && !d.settling() && len(d.pending) == 0 && d.backgroundWorks == 0 && !d.stopped && !d.explicitStopLocked() && !d.canceledOutcome
		d.mu.Unlock()
		if !stillValid {
			return nil
		}
	}
	reportCommitted := false
	if record.Revision != 0 {
		ctx, cancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
		defer cancel()
		coord := m.coordination()
		if deferredReplay && report.ID != "" {
			var commitErr error
			for attempt := 0; attempt < 3; attempt++ {
				commitRecord := record
				commitReport := report
				commitReport.Revision = commitRecord.Revision + 1
				commitErr = coord.CommitChild(ctx, session.ChildCommit{ExpectedRevision: commitRecord.Revision, Record: commitRecord, Reports: []session.ChildReport{commitReport}})
				if commitErr == nil {
					record.Revision++
					report, reportCommitted = commitReport, true
					break
				}

				records, loadErr := coord.LoadChildren(context.WithoutCancel(ctx), record.RootSessionID)
				if loadErr != nil {
					commitErr = errors.Join(commitErr, loadErr)
					break
				}
				var current *session.ChildRecord
				for i := range records {
					if records[i].Node.ID == id && records[i].Node.SessionID == sess.ID {
						current = &records[i]
						break
					}
				}
				if current == nil || current.Node.State == subagent.NodeStopped || current.LastTurnID != turnID {
					commitErr = nil
					break
				}
				record = *current
				pending, pendingErr := coord.PendingReports(context.WithoutCancel(ctx), current.ParentSessionID)
				if pendingErr != nil {
					commitErr = errors.Join(commitErr, pendingErr)
					break
				}
				found := false
				for _, stored := range pending {
					if stored.ID == report.ID && stored.ChildSessionID == sess.ID && stored.TurnID == turnID {
						report, reportCommitted, found = stored, true, true
						break
					}
				}
				if !found {
					accepted, probeErr := coord.AcceptReport(context.WithoutCancel(ctx), current.ParentSessionID, report.ID, nil)
					if probeErr == nil && (accepted.MessageID != 0 || accepted.Message != nil) {
						report.Revision, reportCommitted = current.Revision, true
						found = true
					} else if probeErr != nil && !errors.Is(probeErr, session.ErrNotFound) {
						commitErr = errors.Join(commitErr, probeErr)
						break
					}
				}
				if found {
					commitErr = nil
					break
				}
			}
			if commitErr != nil {
				return commitErr
			}
		} else {
			commit := session.ChildCommit{ExpectedRevision: record.Revision, Record: record}
			if report.ID != "" {
				commit.Reports = []session.ChildReport{report}
			}
			err := coord.CommitChild(ctx, commit)
			if err == nil {
				record.Revision++
				reportCommitted = report.ID != ""
			}
			if errors.Is(err, session.ErrRevisionConflict) {
				reconcileCtx, reconcileCancel := context.WithTimeout(context.WithoutCancel(ctx), defaultSubagentPersistenceTimeout)
				defer reconcileCancel()
				records, loadErr := coord.LoadChildren(reconcileCtx, record.RootSessionID)
				if loadErr != nil {
					return loadErr
				}
				for _, persisted := range records {
					if persisted.Node.ID == id && persisted.LastTurnID == turnID && persisted.Revision >= record.Revision+1 {
						record = persisted
						err = nil
						if report.ID != "" {
							pending, pendingErr := coord.PendingReports(reconcileCtx, persisted.ParentSessionID)
							if pendingErr != nil {
								return pendingErr
							}
							for _, stored := range pending {
								if stored.ID == report.ID && stored.ChildSessionID == sess.ID && stored.TurnID == turnID {
									report, reportCommitted = stored, true
									break
								}
							}
						}
						break
					}
				}
			}
			if err != nil {
				return err
			}
		}
	}

	m.mu.Lock()
	if current := m.children[id]; current == rec {
		current.durable = record
		_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
			if d.deferredCompletionReportTurnID != "" && (reportCommitted && report.ID == childReportID(sess.ID, d.deferredCompletionReportTurnID) || record.LastTurnID != d.deferredCompletionReportTurnID) {
				d.deferredCompletionReportTurnID = ""
			}
			return nil
		})
		_ = m.tree.Update(id, func(n *subagent.Node) {
			n.State, n.Error, n.NeedsAttention = state, runErr, state == subagent.NodeFailed
		})
	}
	m.mu.Unlock()
	if causalWake && reportCommitted {
		reportSnapshot := report
		reportSnapshot.Revision = record.Revision
		_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
			if d.activeRequestID == turnID && d.generation > d.settledGeneration && d.completionInFlight && !d.stopped {
				d.completionReport = &reportSnapshot
			}
			return nil
		})
	}
	transition.Unlock()
	transitionHeld = false
	if deferredReplay && reportCommitted {
		report.Revision = record.Revision
		_ = m.authorizeExecutedTurnReport(context.WithoutCancel(ctx), d, turnID, report)
	}
	parentSession := rec.parentSess
	if parent, ok := m.r.sessionDrivers.Lookup(parentID); ok {
		parentSession = parent.session()
	}
	if parentSession != nil {
		stopCtx, releaseEnv, captureErr := m.r.captureEnvironmentForOperation(m.r.lifetime(), parentSession)
		if captureErr == nil {
			// A runtime without a team has no parent agent, hence no stop hooks to run.
			if t := m.r.teamForContext(stopCtx, parentSession); t != nil {
				a, _ := t.Agent(parentAgent)
				m.r.executeSubagentStopHooks(stopCtx, parentSession, sess, a, sess.AgentName, result)
			}
			releaseEnv()
		}
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

// commitDeferredCompletionReport replays a child completion that settled while
// retained work was still owned. A completion steer can wake and settle a child
// before the process owner closes; when Close drops the last count the normal
// scheduler rechecks this process-local report intent.
func (d *sessionDriver) clearDeferredCompletionReport(ctx context.Context, turnID string) {
	_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
		if d.deferredCompletionReportTurnID == turnID {
			d.deferredCompletionReportTurnID = ""
		}
		return nil
	})
}

func (m *subagentManager) commitDeferredCompletionReport(d *sessionDriver) bool {
	var turnID string
	if err := d.ownerCall(context.WithoutCancel(m.r.lifetime()), func() error {
		if d.running() || d.starting() || d.settling() || len(d.pending) != 0 || d.backgroundWorks != 0 {
			return nil
		}
		if d.stopped || d.explicitStopLocked() || d.canceledOutcome {
			d.deferredCompletionReportTurnID = ""
			return nil
		}
		turnID = d.deferredCompletionReportTurnID
		return nil
	}); err != nil || turnID == "" {
		return false
	}
	// Use the canonical child completion transaction, including revision
	// conflict reconciliation and post-commit causal wake authorization.
	_ = m.completeSessionTurnContext(context.WithoutCancel(m.r.lifetime()), d, turnID, "", false, true)
	var pending bool
	_ = d.ownerCall(context.WithoutCancel(m.r.lifetime()), func() error {
		pending = d.deferredCompletionReportTurnID == turnID
		return nil
	})
	return pending
}

func (g *sessionDriverRegistry) deliverReports(d *sessionDriver) bool {
	if g.r.subagents == nil {
		return false
	}
	var retry *session.ChildReport
	var causalOnly bool
	var causalReports []session.ChildReport
	var dormant bool
	_ = d.ownerCall(g.r.lifetime(), func() error {
		dormant, retry, causalOnly = d.viewDormant, d.reportRetry, d.causalReportOnly
		for _, reportID := range d.causalReportOrder {
			if report, ok := d.causalReports[reportID]; ok {
				causalReports = append(causalReports, report)
			}
		}
		return nil
	})
	if dormant && !causalOnly {
		return false
	}
	if causalOnly {
		for _, report := range causalReports {
			if err := d.acceptAuthorizedReport(g.r.lifetime(), report); err != nil {
				_ = d.ownerCall(context.WithoutCancel(g.r.lifetime()), func() error {
					copy := report
					d.reportRetry = &copy
					if sessionErr, ok := errors.AsType[*SessionError](err); ok && (sessionErr.Kind == SessionErrorInvalid || sessionErr.Kind == SessionErrorStopped || sessionErr.Kind == SessionErrorPersistence) {
						d.events.PublishForRequest(d.sessionIDLocked(), "report:"+report.ID, ErrorForSession(d.sessionIDLocked(), "authorized child completion remains pending: "+err.Error()))
					}
					return nil
				})
				return true
			}
			_ = d.ownerCall(context.WithoutCancel(g.r.lifetime()), func() error {
				if d.reportRetry != nil && d.reportRetry.ID == report.ID {
					d.reportRetry = nil
				}
				return nil
			})
		}
		// A causal-only owner never probes unrelated persisted outbox rows.
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
	return d.acceptReportWithCausalWake(ctx, report, false)
}

func (d *sessionDriver) acceptAuthorizedReport(ctx context.Context, report session.ChildReport) error {
	return d.acceptReportWithCausalWake(ctx, report, true)
}

func (d *sessionDriver) acceptReportWithCausalWake(ctx context.Context, report session.ChildReport, causalWake bool) error {
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
		if d.viewDormant && !causalWake {
			return sessionIOReservation{}, nil
		}
		if causalWake {
			if !d.causalReportOnly || d.causalReports[report.ID].ID != report.ID || d.causalReports[report.ID].ChildSessionID != report.ChildSessionID || d.causalReports[report.ID].ParentSessionID != d.identityID || d.stopped || d.stoppedView || d.explicitStopLocked() {
				return sessionIOReservation{}, &SessionError{Kind: SessionErrorStopped, SessionID: d.identityID, RequestID: "report:" + report.ID, Operation: "accept_report", Detail: "causal report authorization is no longer valid"}
			}
			if d.r.subagents != nil {
				if err := d.r.subagents.sessionAdmissionError(d.identityID); err != nil {
					return sessionIOReservation{}, err
				}
			}
			if err := d.dormantCommunicationAdmissionLocked(QueuedMessage{RequestID: "report:" + report.ID}); err != nil {
				return sessionIOReservation{}, err
			}
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
					if causalWake {
						d.discardCausalReportLocked(report.ID)
					}
					return nil
				}
				position := -1
				for i, item := range d.sess.MessagesSnapshot() {
					if item.Message != nil && item.Message.TurnID == message.TurnID {
						if !item.Message.Pending {
							if causalWake {
								d.discardCausalReportLocked(report.ID)
							}
							return nil
						}
						position = i
						break
					}
				}
				for _, queued := range d.pending {
					if queued.RequestID == message.TurnID {
						if causalWake {
							d.finishCausalReportAdmissionLocked(report.ID)
						}
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
				if causalWake {
					d.finishCausalReportAdmissionLocked(report.ID)
				}
				return nil
			},
		}, nil
	})
	if err == nil {
		d.WakePending()
	}
	return err
}

func (d *sessionDriver) discardCausalReportLocked(reportID string) {
	delete(d.causalReports, reportID)
	d.causalReportOrder = slices.DeleteFunc(d.causalReportOrder, func(id string) bool { return id == reportID })
	if len(d.causalReports) == 0 {
		d.causalReports = nil
	}
}

func (d *sessionDriver) finishCausalReportAdmissionLocked(reportID string) {
	d.authorizeCausalReportViewLocked()
	d.discardCausalReportLocked(reportID)
}

// authorizeExecutedTurnReport is called only after the generation's durable terminal evidence commits.
// The store row alone cannot provide this authority after process restart.
func (m *subagentManager) authorizeExecutedTurnReport(ctx context.Context, d *sessionDriver, turnID string, report session.ChildReport) bool {
	if turnID == "" || report.ID == "" || report.TurnID != turnID {
		return false
	}
	sess := d.session()
	if sess == nil || !sess.AsyncSubagent {
		return false
	}
	m.mu.Lock()
	var rec *childRecord
	for _, candidate := range m.children {
		if candidate.sessionID == sess.ID {
			rec = candidate
			break
		}
	}
	valid := rec != nil && rec.durable.Node.State != subagent.NodeStopped && rec.durable.LastTurnID == turnID && !rec.synchronous
	if valid {
		valid = rec.durable.RootSessionID != "" && rec.durable.Revision == report.Revision && report.ID == childReportID(sess.ID, turnID) && report.ParentSessionID == rec.parentSession && report.ChildSessionID == rec.sessionID
	}
	rootID, parentID, childID := "", "", ""
	if valid {
		rootID, parentID, childID = rec.durable.RootSessionID, rec.parentSession, rec.sessionID
	}
	m.mu.Unlock()
	if !valid || m.r.subagents.sessionAdmissionError(childID) != nil {
		return false
	}
	return m.authorizeCompletionReport(ctx, rootID, parentID, childID, report)
}

// authorizeCompletionReport grants this process a one-report wake based only
// on a freshly committed child turn; restore never reconstructs this authority.
func (m *subagentManager) authorizeCompletionReport(ctx context.Context, rootID, parentID, childID string, report session.ChildReport) bool {
	if report.ID == "" || report.ParentSessionID != parentID || report.ChildSessionID != childID {
		return false
	}
	transition := m.transition(rootID)
	if transition.LockContext(ctx) != nil {
		return false
	}
	defer transition.Unlock()
	m.mu.Lock()
	var child *childRecord
	for _, record := range m.children {
		if record.sessionID == childID {
			child = record
			break
		}
	}
	valid := child != nil && child.parentSession == parentID && child.durable.RootSessionID == rootID && child.durable.Node.State != subagent.NodeStopped && child.durable.LastTurnID == report.TurnID && report.ID == childReportID(childID, report.TurnID) && child.durable.Revision > 0 && report.Revision == child.durable.Revision && !m.sessionStoppingLocked(childID)
	if parent, exists := m.sessions[parentID]; exists && parent != nil {
		valid = valid && (parent.topLevel && parentID == rootID || m.children[parent.node] != nil && m.children[parent.node].sessionID == parentID && m.children[parent.node].durable.RootSessionID == rootID && m.children[parent.node].durable.Node.State != subagent.NodeStopped)
	} else {
		valid = false
	}
	m.mu.Unlock()
	if !valid || m.r.subagents.sessionAdmissionError(parentID) != nil {
		return false
	}
	parent, ok := m.r.sessionDrivers.Lookup(parentID)
	if !ok {
		return false
	}
	var authorized bool
	_ = parent.ownerCall(context.WithoutCancel(ctx), func() error {
		if parent.stopped || parent.stoppedView || parent.explicitStopLocked() || parent.r.subagents.sessionAdmissionError(parentID) != nil {
			return nil
		}
		if !parent.viewDormant && !parent.causalReportOnly {
			return nil
		}
		if parent.causalReports == nil {
			parent.causalReports = make(map[string]session.ChildReport)
		}
		if _, exists := parent.causalReports[report.ID]; !exists {
			parent.causalReportOrder = append(parent.causalReportOrder, report.ID)
		}
		parent.causalReports[report.ID] = report
		parent.causalReportOnly = true
		authorized = true
		return nil
	})
	if authorized {
		parent.r.sessionDrivers.signalWork()
	}
	return authorized
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

// chooseCompletionResult picks the completion preview. The live generation result is
// the newest answer; the durable record is consulted only for a deferred replay whose
// live result is empty.
func chooseCompletionResult(live, durable string, deferredReplay bool) string {
	if deferredReplay && live == "" {
		return durable
	}
	return live
}
