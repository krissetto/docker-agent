package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// subagentManager supervises async subagents above the per-session driver
// layer. It owns tree metadata, parent/child linkage, latest results, and
// reporting; each child session's run/wake/delivery lifecycle is owned by its
// sessionDriver.
type subagentManager struct {
	r      *LocalRuntime
	tree   *subagent.Tree
	ctx    context.Context //nolint:containedctx // long-lived context bounding detached child sessions; cancelled by Close.
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu        sync.Mutex
	metricsMu sync.Mutex
	persistMu sync.Mutex
	persist   *subagentPersistence
	coord     session.CoordinationStore
	closed    bool
	sessions  map[string]*sessionSubagents
	children  map[subagent.NodeID]*childRecord

	restoreMu sync.Mutex
}

// sessionSubagents tracks the outstanding subagents of one session and the tree
// node that represents it (so subagents it spawns are linked under the right
// parent at any depth). For top-level sessions (synthetic root node) the session
// object is retained so tree snapshots can be mirrored onto it for persistence.
type sessionSubagents struct {
	node     subagent.NodeID
	topLevel bool
	sess     *session.Session
	unwatch  func()
}

// childRecord retains what read_subagent / send_message / reporting need about
// a subagent. The sessionDriver owns run/wake/cancel state; unwatch releases
// the manager's completion callback when the child is stopped.
type childRecord struct {
	name            string
	parentSession   string
	parentAgentName string
	sessionID       string
	session         *session.Session
	parentSess      *session.Session
	agent           *agent.Agent
	unwatch         func()

	durable session.ChildRecord
	state   subagent.NodeState
	result  string
	errMsg  string
}

func newSubagentManager(r *LocalRuntime) *subagentManager {
	ctx, cancel := context.WithCancel(r.ctx())
	return &subagentManager{
		r:        r,
		tree:     subagent.NewTree(),
		ctx:      ctx,
		cancel:   cancel,
		sessions: map[string]*sessionSubagents{},
		children: map[subagent.NodeID]*childRecord{},
	}
}

func (m *subagentManager) persistence() *subagentPersistence {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	if m.persist == nil {
		m.persist = newSubagentPersistence(m.r.subagentStore, m.r.sessionStore)
	}
	return m.persist
}

func (m *subagentManager) updateSessionMetrics(sess *session.Session, toolCalls int64) {
	if sess == nil {
		return
	}
	input, output := sess.Usage()
	cost := sess.OwnCost()
	m.mu.Lock()
	tracked := m.sessions[sess.ID]
	var id subagent.NodeID
	if tracked != nil {
		id = tracked.node
	}
	m.mu.Unlock()
	if id == "" {
		m.ensureRoot(sess, sess.AgentName)
		id = subagent.SessionRootID(sess.ID)
	}
	// Serialize the metric mutation with snapshot capture/enqueue. Otherwise an
	// older concurrent full-tree snapshot can be persisted after a newer one.
	m.metricsMu.Lock()
	defer m.metricsMu.Unlock()
	_ = m.tree.Update(id, func(node *subagent.Node) {
		// Concurrent turns can finish metric snapshots out of order. Cumulative
		// counters never regress; toolCalls is the delta for this completion.
		node.Cost = max(node.Cost, cost)
		node.InputTokens = max(node.InputTokens, input)
		node.OutputTokens = max(node.OutputTokens, output)
		node.ToolCalls += toolCalls
	})
	m.persistSnapshotLocked(true)
}

func (m *subagentManager) Tree() *subagent.Tree { return m.tree }

// ensureSessionLocked returns (creating if needed) the tracking record for a
// session. node is the tree node representing the session; when empty (a
// top-level session) a synthetic root node is created. Caller holds m.mu.
func (m *subagentManager) ensureSessionLocked(sess *session.Session, agentName string, node subagent.NodeID) *sessionSubagents {
	if st, ok := m.sessions[sess.ID]; ok {
		return st
	}
	st := &sessionSubagents{node: node}
	if node == "" {
		st.node = subagent.SessionRootID(sess.ID)
		st.topLevel = true
		st.sess = sess

		state := subagent.NodeRunning
		if m.r.sessionDrivers != nil {
			if driver, ok := m.r.sessionDrivers.Lookup(sess.ID); ok && driver.Status().State != SessionStateRunning {
				state = subagent.NodeIdle
			}
		}
		_ = m.tree.Add(subagent.Node{ID: st.node, Agent: agentName, State: state})
		if m.r.sessionDrivers != nil {
			if driver, ok := m.r.sessionDrivers.Lookup(sess.ID); ok {
				st.sess = nil
				sessionID := sess.ID
				unwatchStarted := driver.OnStarted(func() { m.updateRootLifecycle(sessionID, subagent.NodeRunning, "") })
				unwatchSettled := driver.OnSettled(func() {
					current, ok := m.r.sessionDrivers.Lookup(sessionID)
					if !ok {
						return
					}
					if lastErr := current.LastError(); lastErr != "" {
						m.updateRootLifecycle(sessionID, subagent.NodeFailed, lastErr)
						return
					}
					m.updateRootLifecycle(sessionID, subagent.NodeIdle, "")
				})
				st.unwatch = func() { unwatchStarted(); unwatchSettled() }
			}
		}
	}
	m.sessions[sess.ID] = st
	return st
}

// persistSnapshot mirrors each root's subtree onto its owning top-level
// session and store row. A runtime can serve multiple root sessions, so each
// owner must receive only its own swarm.
func (m *subagentManager) persistSnapshot() {
	m.persistSnapshotLocked(false)
}

func (m *subagentManager) persistSnapshotLocked(alreadyLocked bool) {
	if !alreadyLocked {
		m.metricsMu.Lock()
		defer m.metricsMu.Unlock()
	}
	full := m.tree.Snapshot()
	full.Durability = m.r.sessionDurability()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	type projection struct {
		id   string
		sess *session.Session
		snap subagent.Snapshot
	}
	var projections []projection
	for id, tracked := range m.sessions {
		if !tracked.topLevel {
			continue
		}
		if snap, ok := snapshotForRoot(full, subagent.SessionRootID(id)); ok {
			projections = append(projections, projection{id, tracked.sess, snap})
		}
	}
	m.mu.Unlock()
	for _, item := range projections {
		if m.r.sessionDrivers != nil {
			if d, ok := m.r.sessionDrivers.Lookup(item.id); ok {
				item.sess = d.session()
			}
		}
		if item.sess != nil {
			item.sess.SetSubagentTree(&item.snap)
		}
		if _, ok := m.r.subagentStore.(*subagent.InMemoryStore); ok {
			_ = m.r.subagentStore.SaveTree(context.WithoutCancel(m.ctx), item.id, item.snap)
		} else if persist := m.persistence(); persist != nil {
			persist.enqueueTree(item.id, item.snap)
		}
	}
}

func snapshotForRoot(full subagent.Snapshot, rootID subagent.NodeID) (subagent.Snapshot, bool) {
	for _, root := range full.Nodes {
		if root.Node.ID == rootID {
			return subagent.Snapshot{Version: subagent.SnapshotVersion, Durability: full.Durability, Root: rootID, Nodes: []subagent.NodeSnapshot{root}}, true
		}
	}
	return subagent.Snapshot{}, false
}

func (m *subagentManager) ensureRoot(sess *session.Session, agentName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureSessionLocked(sess, agentName, "")
}

func (m *subagentManager) updateRootLifecycle(sessionID string, state subagent.NodeState, errMsg string) {
	m.mu.Lock()
	tracked := m.sessions[sessionID]
	if tracked == nil || !tracked.topLevel || m.closed {
		m.mu.Unlock()
		return
	}
	nodeID := tracked.node
	m.mu.Unlock()
	_ = m.tree.Update(nodeID, func(node *subagent.Node) {
		node.State = state
		node.Error = errMsg
		node.NeedsAttention = state == subagent.NodeFailed
		if state == subagent.NodeFailed {
			node.WaitingOn = "failed"
		} else {
			node.WaitingOn = ""
		}
	})
	m.persistSnapshot()
}

func (m *subagentManager) registerIdleChild(parent *session.Session, parentAgent string, child *session.Session, target *agent.Agent, ref subagent.AllowedSubagent) error {
	_, err := m.admitChild(parent, parentAgent, child, target, ref, "")
	return err
}

func (m *subagentManager) Spawn(parent *session.Session, parentAgent string, ref subagent.AllowedSubagent, task string) (subagent.NodeID, error) {
	m.mu.Lock()
	err := m.spawnAdmissionErrorLocked(parent)
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	target, err := m.r.team.Agent(ref.Agent)
	if err != nil {
		return "", err
	}
	approved, policy, permissions := parent.SafetySettings()
	child := newSubSession(parent, SubSessionConfig{AgentName: ref.Agent, ToolsApproved: approved, SafetyPolicy: policy, Permissions: permissions, NonInteractive: true, PinAgent: true}, target)
	child.AsyncSubagent = true
	turnID, err := newSessionRequestID()
	if err != nil {
		return "", err
	}
	input := session.UserMessage(task)
	input.Pending, input.Accepted, input.TurnID = true, true, turnID
	input.InputOrigin, input.SenderID, input.SenderName, input.InputMode = session.InputOriginAgent, parent.ID, parentAgent, "turn"
	child.AddMessage(input)
	id, err := m.admitChild(parent, parentAgent, child, target, ref, task)
	if err != nil {
		return "", err
	}
	if d, ok := m.r.sessionDrivers.Lookup(child.ID); ok {
		d.WakePending()
	}
	return id, nil
}

func (m *subagentManager) admitChild(parent *session.Session, parentAgent string, child *session.Session, target *agent.Agent, ref subagent.AllowedSubagent, task string) (subagent.NodeID, error) {
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	if m.r.sessionStore != nil {
		if _, err := m.r.sessionStore.GetSession(m.ctx, parent.ID); errors.Is(err, session.ErrNotFound) {
			parent.SetAttribute(SessionAgentAttribute, parentAgent)
			if err := m.r.sessionStore.AddSession(m.ctx, parent.OwnSnapshot()); err != nil && !errors.Is(err, session.ErrAlreadyExists) {
				return "", err
			}
		}
	}
	child.SetAttribute(SessionAgentAttribute, ref.Agent)
	child.SetAttribute(SessionParentAgentAttribute, parentAgent)
	reservation, err := m.r.sessionDrivers.PrepareRestore(m.ctx, child)
	if err != nil {
		return "", err
	}
	defer reservation.Discard()
	if task != "" {
		if err := m.r.sessionDrivers.admitRun(reservation.driver); err != nil {
			return "", err
		}
	}
	m.mu.Lock()
	if err := m.spawnAdmissionErrorLocked(parent); err != nil {
		m.mu.Unlock()
		return "", err
	}
	tracked := m.ensureSessionLocked(parent, parentAgent, "")
	id := m.tree.NewNodeID()
	node := subagent.Node{ID: id, Parent: tracked.node, SessionID: child.ID, Agent: ref.Agent, Name: ref.Name, Description: ref.Description, State: subagent.NodeIdle}
	node.Task, _ = subagent.PreviewText(task, subagent.PreviewLen)
	record := session.ChildRecord{RootSessionID: m.rootSessionLocked(parent.ID), ParentSessionID: parent.ID, Node: node, Revision: 1}
	rec := &childRecord{name: ref.DisplayName(), parentSession: parent.ID, parentAgentName: parentAgent, sessionID: child.ID, agent: target, state: subagent.NodeIdle, durable: record}
	d := reservation.driver
	d.SetPreStartErrorGate(func() error { return m.admitChildRun(id) }, nil)
	rec.unwatch = d.OnStarted(func() { m.markChildRunning(id) })
	err = m.r.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, func() error {
		if err := m.admitDurableChild(parent, child, record); err != nil {
			return err
		}
		if err := m.tree.Add(node); err != nil {
			return err
		}
		m.children[id] = rec
		m.sessions[child.ID] = &sessionSubagents{node: id}
		return nil
	})
	if err == nil && task != "" && m.r.TitleGenerator(m.ctx) != nil {
		m.wg.Go(func() { m.startChildTitle(child, task) })
	}
	m.mu.Unlock()
	if err != nil {
		rec.unwatch()
		return "", err
	}
	m.persistSnapshot()
	if persistence := m.persistence(); persistence != nil {
		if err := persistence.flushNow(); err != nil {
			slog.WarnContext(m.ctx, "Failed to checkpoint admitted topology", "error", err)
		}
	}
	return id, nil
}

type childAdmissionDenial string

const (
	childAdmissionParentStopped childAdmissionDenial = "parent_stopped"
	childAdmissionDepth         childAdmissionDenial = "depth"
	childAdmissionMissing       childAdmissionDenial = "missing"
	childAdmissionStopped       childAdmissionDenial = "stopped"
)

type childAdmissionRequest struct {
	parentSession string
	childID       subagent.NodeID
	spawn         bool
}

type childAdmissionDecision struct {
	denial        childAdmissionDenial
	parentSession string
	limit         int
}

func (m *subagentManager) childAdmissionLocked(request childAdmissionRequest) childAdmissionDecision {
	parentSession := request.parentSession
	if request.spawn {
		if parentSession == "" || !m.parentAcceptsSpawnLocked(parentSession) {
			return childAdmissionDecision{denial: childAdmissionParentStopped, parentSession: parentSession}
		}
	} else {
		rec := m.children[request.childID]
		if rec == nil {
			return childAdmissionDecision{denial: childAdmissionMissing}
		}
		parentSession = rec.parentSession
		if rec.state == subagent.NodeStopped || m.closed {
			return childAdmissionDecision{denial: childAdmissionStopped, parentSession: parentSession}
		}
		if rec.state == subagent.NodeStarting {
			return childAdmissionDecision{}
		}
	}

	if request.spawn && !limitAllows(m.sessionDepthLocked(parentSession), m.r.maxSubagentDepth) {
		return childAdmissionDecision{denial: childAdmissionDepth, parentSession: parentSession, limit: m.r.maxSubagentDepth}
	}
	return childAdmissionDecision{parentSession: parentSession}
}

func (m *subagentManager) childAdmissionError(request childAdmissionRequest, decision childAdmissionDecision) error {
	switch decision.denial {
	case "":
		return nil
	case childAdmissionParentStopped:
		return errors.New("parent session is stopped")
	case childAdmissionDepth:
		return &SessionError{Kind: SessionErrorCapacity, SessionID: decision.parentSession, Operation: "subagent_depth", Limit: decision.limit}
	case childAdmissionMissing:
		return &SessionError{Kind: SessionErrorNotFound, Operation: "admit_child_run"}
	case childAdmissionStopped:
		sessionID := ""
		if rec := m.children[request.childID]; rec != nil {
			sessionID = rec.sessionID
		}
		return &SessionError{Kind: SessionErrorStopped, SessionID: sessionID, Operation: "admit_child_run"}
	default:
		return &SessionError{Kind: SessionErrorInvalid, Operation: "subagent_admission"}
	}
}

func (m *subagentManager) spawnAdmissionErrorLocked(parentSess *session.Session) error {
	parentSession := ""
	if parentSess != nil {
		parentSession = parentSess.ID
	}
	request := childAdmissionRequest{parentSession: parentSession, spawn: true}
	return m.childAdmissionError(request, m.childAdmissionLocked(request))
}

func activeSubagentState(state subagent.NodeState) bool {
	return state == subagent.NodeStarting || state == subagent.NodeRunning
}

// admitChildRun atomically reserves capacity by moving a quiescent session to
// Starting. Starting -> Running does not consume a second slot.
func (m *subagentManager) admitChildRun(childID subagent.NodeID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	request := childAdmissionRequest{childID: childID}
	return m.childAdmissionError(request, m.childAdmissionLocked(request))
}

func (m *subagentManager) abortChildStart(subagent.NodeID) { m.signalCapacityRelease() }

// signalCapacityRelease coalesces admission opportunities. The worker is
// started lazily so focused tests that build a manager literal get the same
// lifecycle behavior as production constructors.
func (m *subagentManager) signalCapacityRelease() { m.r.sessionDrivers.signalWork() }

func (m *subagentManager) rootSessionLockedSafe(sessionID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rootSessionLocked(sessionID)
}

func (m *subagentManager) rootSessionLocked(sessionID string) string {
	seen := map[string]bool{}
	for sessionID != "" && !seen[sessionID] {
		seen[sessionID] = true
		parent := ""
		for _, rec := range m.children {
			if rec.sessionID == sessionID {
				parent = rec.parentSession
				break
			}
		}
		if parent == "" {
			return sessionID
		}
		sessionID = parent
	}
	return sessionID
}

func (m *subagentManager) sessionDepthLocked(sessionID string) int {
	depth := 0
	seen := map[string]bool{}
	for sessionID != "" && !seen[sessionID] {
		seen[sessionID] = true
		parent := ""
		for _, rec := range m.children {
			if rec.sessionID == sessionID {
				parent = rec.parentSession
				break
			}
		}
		if parent == "" {
			return depth
		}
		depth++
		sessionID = parent
	}
	return depth
}

func (m *subagentManager) parentAcceptsSpawnLocked(sessionID string) bool {
	if m.closed {
		return false
	}
	if m.r.sessionDrivers != nil {
		m.r.sessionDrivers.mu.Lock()
		_, deleted := m.r.sessionDrivers.deleted[sessionID]
		m.r.sessionDrivers.mu.Unlock()
		if deleted {
			return false
		}
	}
	for _, rec := range m.children {
		if rec.sessionID == sessionID {
			return rec.state != subagent.NodeStopped
		}
	}
	return true
}

// startChildTitle titles a freshly spawned subagent session from its first
// user message, exactly like a root session's first prompt. The caller runs it
// in manager-owned lifecycle work. Best-effort: failures never affect the run.
func (m *subagentManager) startChildTitle(childSess *session.Session, task string) {
	gen := m.r.TitleGenerator(m.ctx)
	if gen == nil {
		return
	}
	title, err := gen.Generate(m.ctx, childSess.ID, []string{task})
	if err != nil || title == "" {
		slog.DebugContext(m.ctx, "Subagent title generation skipped", "session_id", childSess.ID, "error", err)
		return
	}
	// The store row may not exist yet (the child persists after its first
	// turn); UpdateSessionTitle still sets the in-memory title, which the
	// first persist then writes.
	if err := m.r.UpdateSessionTitle(m.ctx, childSess, title); err != nil {
		slog.DebugContext(m.ctx, "Subagent title not yet persistable", "session_id", childSess.ID, "error", err)
	}
}

// markChildRunning records that a child driver's turn has started. This keeps
// the swarm tree/TUI in sync when a previously-idle subagent is woken by a
// descendant report or other detached input.
func (m *subagentManager) markChildRunning(childID subagent.NodeID) {
	m.mu.Lock()
	rec := m.children[childID]
	if rec == nil || rec.state == subagent.NodeStopped {
		m.mu.Unlock()
		return
	}
	rec.state = subagent.NodeRunning
	_ = m.tree.Update(childID, func(n *subagent.Node) { n.State = subagent.NodeRunning })
	m.mu.Unlock()
	m.persistSnapshot()
}

// stopChild explicitly finalizes a subagent of parentID: it cancels any
// in-flight run, releases callbacks, and stops its own subagents recursively.
// Stopped subagents keep their record so read_subagent still works, but accept
// no future input.
func (m *subagentManager) stopChild(parentID string, id subagent.NodeID) (string, error) {
	m.mu.Lock()
	rec := m.children[id]
	if rec == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("no subagent with id %q", id)
	}
	if rec.parentSession != parentID {
		m.mu.Unlock()
		return "", fmt.Errorf("subagent %q is not one of yours", id)
	}
	if rec.state == subagent.NodeStopped {
		m.mu.Unlock()
		return "", fmt.Errorf("subagent %q is already stopped", id)
	}
	stopped := m.markStoppedLocked(rec, id, nil)
	name := rec.name
	m.mu.Unlock()
	m.signalCapacityRelease()

	for _, stoppedRec := range stopped {
		if stoppedRec.unwatch != nil {
			stoppedRec.unwatch()
		}
	}
	for _, stoppedRec := range stopped {
		m.r.sessionDrivers.StopAll(stoppedRec.sessionID)
	}
	for _, stoppedRec := range stopped {
		if d, ok := m.r.sessionDrivers.Lookup(stoppedRec.sessionID); ok {
			d.Wait()
		}
	}

	for _, stoppedRec := range stopped {
		if stoppedRec.durable.Revision == 0 {
			continue
		}
		record := stoppedRec.durable
		record.Node.State = subagent.NodeStopped
		if err := m.coordination().CommitChild(context.WithoutCancel(m.ctx), session.ChildCommit{ExpectedRevision: record.Revision, Record: record}); err != nil {
			return "", err
		}
		record.Revision++
		m.mu.Lock()
		stoppedRec.durable = record
		m.mu.Unlock()
	}
	m.persistSnapshot()
	return name, nil
}

// markStoppedLocked records a stopped subtree without blocking on driver
// shutdown. Caller holds m.mu. Hook cleanup is deferred to the caller so it
// never acquires d.mu while m.mu is held.
func (m *subagentManager) markStoppedLocked(rec *childRecord, id subagent.NodeID, stopped []*childRecord) []*childRecord {
	rec.state = subagent.NodeStopped
	stopped = append(stopped, rec)
	_ = m.tree.Update(id, func(n *subagent.Node) { n.State = subagent.NodeStopped; n.NeedsAttention = false; n.WaitingOn = "" })

	for childID, child := range m.children {
		if child.parentSession == rec.sessionID && child.state != subagent.NodeStopped {
			stopped = m.markStoppedLocked(child, childID, stopped)
		}
	}
	return stopped
}

// deliver routes input to a subagent's session driver. The manager validates
// stopped/ownership state before calling this, so a known idle child may wake.
func (m *subagentManager) deliver(sessionID, content string) bool {
	return m.deliverAgent(sessionID, content, "", "")
}

func (m *subagentManager) deliverAgent(sessionID, content, senderID, senderName string) bool {
	id, err := newSessionRequestID()
	if err != nil {
		return false
	}
	return m.r.sessionDrivers.PostKnown(m.ctx, sessionID, QueuedMessage{Content: content, RequestID: id, InputOrigin: session.InputOriginAgent, SenderID: senderID, SenderName: senderName, InputMode: "steer"}, true)
}

// deliverExplicitToParent preserves child-to-top-level delivery when the
// parent's driver has not been observed yet, but otherwise uses direct
// admission semantics so capacity denial is surfaced to the tool caller.
func (m *subagentManager) deliverExplicitToParent(sessionID, content string, sender ...string) bool {
	var senderID, senderName string
	if len(sender) == 2 {
		senderID, senderName = sender[0], sender[1]
	}
	id, err := newSessionRequestID()
	if err != nil {
		return false
	}
	return m.r.sessionDrivers.PostOrBuffer(m.ctx, sessionID, QueuedMessage{Content: content, RequestID: id, InputOrigin: session.InputOriginAgent, SenderID: senderID, SenderName: senderName, InputMode: "steer"}, true)
}

// systemInfo wraps a runtime-authored note so the model can distinguish
// harness/system information from ordinary conversation content.
func systemInfo(body string) string {
	return subagent.WrapSystemInfo(body)
}

// nodeForSession returns the subagent node backing a child session, if any.
func (m *subagentManager) nodeForSession(sessionID string) (subagent.NodeID, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rec := range m.children {
		if rec.sessionID == sessionID {
			return id, true
		}
	}
	return "", false
}

// ensureChildDriver lazily rebuilds an evicted persistent child's driver. The
// restore gate serializes rebinds so concurrent send/attach callers cannot
// publish duplicate driver generations.
func (m *subagentManager) ensureChildDriver(ctx context.Context, id subagent.NodeID) error {
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()

	m.mu.Lock()
	rec := m.children[id]
	if rec == nil {
		m.mu.Unlock()
		return fmt.Errorf("no subagent with id %q", id)
	}
	if rec.state == subagent.NodeStopped {
		m.mu.Unlock()
		return fmt.Errorf("subagent %q has been stopped; spawn a new one", id)
	}
	sessionID, parentSession := rec.sessionID, rec.parentSession
	var retained *session.Session
	if rec.session != nil {
		retained = rec.session.Clone()
	}
	m.mu.Unlock()
	if _, ok := m.r.sessionDrivers.Lookup(sessionID); ok {
		return nil
	}

	fail := func(err error) error {
		restoreErr := fmt.Errorf("subagent %q is unreachable: restore persistent session: %w", id, err)
		m.mu.Lock()
		if current := m.children[id]; current != nil && current.state != subagent.NodeStopped {
			current.state = subagent.NodeFailed
			current.errMsg = restoreErr.Error()
			current.unwatch = nil
			_ = m.tree.Update(id, func(node *subagent.Node) {
				node.State = subagent.NodeFailed
				node.Error = restoreErr.Error()
				node.NeedsAttention = true
				node.WaitingOn = "failed"
			})
		}
		m.mu.Unlock()
		m.persistSnapshot()
		return restoreErr
	}
	var loaded *session.Session
	if retained != nil {
		loaded = retained
	} else {
		if m.r.sessionStore == nil || sessionID == "" {
			return fail(errors.New("session snapshot and store are unavailable"))
		}
		var err error
		loaded, err = m.r.sessionStore.GetSession(ctx, sessionID)
		if err != nil {
			return fail(err)
		}
	}
	if loaded == nil || loaded.ID != sessionID || loaded.ParentID != parentSession {
		return fail(errors.New("child session has invalid identity or parent binding"))
	}
	loaded = loaded.Clone()
	loaded.NonInteractive = true
	loaded.AsyncSubagent = true

	childAgent, err := m.r.team.Agent(loaded.AgentName)
	if err != nil {
		return fail(err)
	}
	reservation, err := m.r.sessionDrivers.PrepareRestore(ctx, loaded)
	if err != nil {
		return fail(err)
	}
	defer reservation.Discard()
	driver := reservation.driver
	driver.SetPreStartErrorGate(func() error { return m.admitChildRun(id) }, func() { m.abortChildStart(id) })
	unwatch := driver.OnStarted(func() { m.markChildRunning(id) })

	m.mu.Lock()
	current := m.children[id]
	if m.closed || current == nil || current.state == subagent.NodeStopped {
		m.mu.Unlock()
		unwatch()
		_ = m.r.sessionDrivers.ReleaseDriver(context.WithoutCancel(ctx), sessionID, driver)
		return fmt.Errorf("subagent %q is no longer available", id)
	}
	if err := m.r.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, nil); err != nil {
		m.mu.Unlock()
		unwatch()
		return fail(err)
	}
	oldUnwatch := current.unwatch
	current.session = nil
	current.agent = childAgent
	current.unwatch = unwatch
	m.sessions[sessionID] = &sessionSubagents{node: id}
	m.mu.Unlock()
	if oldUnwatch != nil {
		oldUnwatch()
	}
	return nil
}

// sendToChild delivers a message to a subagent of parentID, returning its
// display name for attribution. Subagents stay conversational after they
// respond: any non-stopped child accepts input (an idle one is re-run). It
// errors when the id is unknown, not a child of the caller, or stopped.
func (m *subagentManager) sendToChild(parentID string, id subagent.NodeID, body string) (string, error) {
	m.mu.Lock()
	rec := m.children[id]
	if rec == nil {
		m.mu.Unlock()
		return "", fmt.Errorf("no subagent with id %q", id)
	}
	if rec.parentSession != parentID {
		m.mu.Unlock()
		return "", fmt.Errorf("subagent %q is not one of yours", id)
	}
	if rec.state == subagent.NodeStopped {
		m.mu.Unlock()
		return "", fmt.Errorf("subagent %q has been stopped; spawn a new one", id)
	}
	sessionID, name := rec.sessionID, rec.name
	m.mu.Unlock()

	if err := m.ensureChildDriver(m.ctx, id); err != nil {
		return "", err
	}
	senderName := ""
	if parent, ok := m.r.sessionDrivers.Lookup(parentID); ok {
		senderName = parent.AgentName()
	}
	if !m.deliverAgent(sessionID, body, parentID, senderName) {
		// Capacity reclamation can race the first lookup. Rebind once more and
		// retry rather than reporting a persistent child as permanently gone.
		if err := m.ensureChildDriver(m.ctx, id); err != nil || !m.deliverAgent(sessionID, body, parentID, senderName) {
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("subagent %q is no longer reachable after restoration", id)
		}
	}
	return name, nil
}

// hasRunningSubagents reports whether a session has descendants actively
// running a turn (running/starting) anywhere in its subagent subtree. Idle,
// failed, and stopped children don't count.
func (m *subagentManager) hasRunningSubagents(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasRunningSubagentsLocked(sessionID)
}

// hasRunningSubagentsLocked is recursive; caller holds m.mu.
func (m *subagentManager) hasRunningSubagentsLocked(sessionID string) bool {
	for _, rec := range m.children {
		if rec.parentSession != sessionID {
			continue
		}
		if m.r.sessionDrivers != nil {
			if d, ok := m.r.sessionDrivers.Lookup(rec.sessionID); ok {
				d.mu.Lock()
				active := d.running || d.starting || d.settling || len(d.pending) != 0
				d.mu.Unlock()
				if active {
					return true
				}
			}
		}
		if rec.sessionID != "" && m.hasRunningSubagentsLocked(rec.sessionID) {
			return true
		}
	}
	return false
}

// readChild returns a snapshot of a direct subagent of parentID. It errors when
// the id is unknown or belongs to another session.
func (m *subagentManager) readChild(parentID string, id subagent.NodeID) (childRecord, error) {
	rec, ok := m.Read(id)
	if !ok {
		return childRecord{}, fmt.Errorf("no subagent with id %q", id)
	}
	if rec.parentSession != parentID {
		return childRecord{}, fmt.Errorf("subagent %q is not one of yours", id)
	}
	return rec, nil
}

func (m *subagentManager) Read(id subagent.NodeID) (childRecord, bool) {
	m.mu.Lock()
	rec, ok := m.children[id]
	if !ok {
		m.mu.Unlock()
		return childRecord{}, false
	}
	snapshot := *rec
	m.mu.Unlock()
	if d, ok := m.r.sessionDrivers.Lookup(snapshot.sessionID); ok {
		snapshot.session = d.session()
	} else if m.r.sessionStore != nil {
		snapshot.session, _ = m.r.sessionStore.GetSession(m.r.lifetime(), snapshot.sessionID)
	}
	if snapshot.session != nil {
		snapshot.result = ownAssistantResult(snapshot.session)
	}
	return snapshot, true
}

func (m *subagentManager) deleteSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	var stopped []*childRecord
	var unwatches []func()
	deleting := map[string]bool{sessionID: true}
	pending := []string{sessionID}
	for len(pending) != 0 {
		parentID := pending[0]
		pending = pending[1:]
		for _, rec := range m.children {
			if rec.parentSession == parentID && !deleting[rec.sessionID] {
				deleting[rec.sessionID] = true
				pending = append(pending, rec.sessionID)
				stopped = append(stopped, rec)
			}
		}
	}
	for id, rec := range m.children {
		if deleting[rec.sessionID] {
			rec.state = subagent.NodeStopped
			_ = m.tree.Update(id, func(node *subagent.Node) {
				node.State, node.NeedsAttention, node.WaitingOn = subagent.NodeStopped, false, ""
			})
			if rec.unwatch != nil {
				unwatches = append(unwatches, rec.unwatch)
				rec.unwatch = nil
			}
		}
	}
	// Reserve the entire cascade before releasing manager admission, including
	// descendants already stopped by an earlier, interrupted delete.
	m.r.sessionDrivers.mu.Lock()
	for id := range deleting {
		m.r.sessionDrivers.deleted[id] = struct{}{}
	}
	m.r.sessionDrivers.mu.Unlock()
	m.mu.Unlock()
	for _, unwatch := range unwatches {
		unwatch()
	}
	for _, rec := range stopped {
		m.r.sessionEvents.Delete(rec.sessionID)
		m.r.sessionDrivers.StopAll(rec.sessionID)
	}
	for _, rec := range stopped {
		if err := m.r.sessionDrivers.Delete(ctx, rec.sessionID); err != nil {
			return err
		}
	}
	// Late metrics and lifecycle callbacks still need their original topology
	// until every descendant publisher has quiesced.
	m.metricsMu.Lock()
	m.mu.Lock()
	for id := range deleting {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	m.metricsMu.Unlock()
	for _, rec := range stopped {
		m.r.interactions.deleteSession(rec.sessionID)
		if m.r.sessionStore != nil {
			_ = m.r.sessionStore.DeleteSession(ctx, rec.sessionID)
		}
	}
	m.persistSnapshot()
	if p := m.persistence(); p != nil {
		return p.flushNow()
	}
	return nil
}

// Close cancels every running subagent and waits for their goroutines to exit.
func (m *subagentManager) closeAdmission() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *subagentManager) CloseContext(ctx context.Context) error {
	m.closeAdmission()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	m.mu.Lock()
	for _, tracked := range m.sessions {
		if tracked.unwatch != nil {
			tracked.unwatch()
			tracked.unwatch = nil
		}
	}
	m.mu.Unlock()
	m.persistMu.Lock()
	persist := m.persist
	m.persistMu.Unlock()
	if persist != nil {
		return persist.close()
	}
	return nil
}

func (m *subagentManager) Close() { _ = m.CloseContext(context.Background()) }

// allowedFromAgent builds the spawn allow-list for an agent from its declared
// async subagents, resolving each target's description from the team when the
// ref does not override it.
func (r *LocalRuntime) allowedFromAgent(a *agent.Agent) []subagent.AllowedSubagent {
	refs := a.AsyncSubagents()
	allowed := make([]subagent.AllowedSubagent, 0, len(refs))
	for _, ref := range refs {
		target, err := r.team.Agent(ref.Agent)
		if err != nil || target == nil {
			slog.WarnContext(r.ctx(), "Ignoring unresolved async subagent reference", "agent", a.Name(), "subagent", ref.Agent, "error", err)
			continue
		}
		desc := ref.Description
		if desc == "" {
			desc = target.Description()
		}
		allowed = append(allowed, subagent.AllowedSubagent{
			Agent:       ref.Agent,
			Name:        ref.Name,
			Description: desc,
		})
	}
	return allowed
}

// SubagentTree returns the live async subagent swarm for this runtime, used by
// the TUI to render a running-subagent tree. Never nil.
func (r *LocalRuntime) SubagentTree() *subagent.Tree {
	return r.subagents.Tree()
}

func (m *subagentManager) setAttention(sessionID, waitingOn string) {
	m.mu.Lock()
	var id subagent.NodeID
	if tracked := m.sessions[sessionID]; tracked != nil {
		id = tracked.node
	}
	if id == "" {
		for nodeID, rec := range m.children {
			if rec.sessionID == sessionID {
				id = nodeID
				break
			}
		}
	}
	if id != "" {
		_ = m.tree.Update(id, func(n *subagent.Node) {
			if n.State == subagent.NodeFailed {
				n.NeedsAttention, n.WaitingOn = true, "failed"
				return
			}
			n.WaitingOn, n.NeedsAttention = waitingOn, waitingOn != ""
		})
	}
	m.mu.Unlock()
	if id != "" {
		m.persistSnapshot()
	}
}

// HasRunningSubagents reports whether sessionID owns any subagents that are
// actively running a turn.
func (r *LocalRuntime) HasRunningSubagents(sessionID string) bool {
	return r.subagents.hasRunningSubagents(sessionID)
}

// SubagentAttachInfo describes what a UI needs to attach a live view to an
// async subagent's sub-session.
type SubagentAttachInfo struct {
	NodeID          subagent.NodeID
	Agent           string // agent running the sub-session
	Name            string // display name (ref name or agent)
	Session         *session.Session
	ParentSessionID string
	ParentAgent     string
}

// SubagentAttachInfo resolves a subagent node id to its attach info. The
// returned session is the live object the manager drives — viewers must treat
// it as read-only. ok is false for unknown ids or subagents without a
// session (failed spawns, unresumable restores).
func (r *LocalRuntime) SubagentAttachInfo(id subagent.NodeID) (SubagentAttachInfo, bool) {
	rec, ok := r.subagents.Read(id)
	if !ok {
		return SubagentAttachInfo{}, false
	}
	if rec.state != subagent.NodeStopped {
		if err := r.subagents.ensureChildDriver(r.ctx(), id); err != nil {
			return SubagentAttachInfo{}, false
		}
		rec, ok = r.subagents.Read(id)
		if !ok || rec.session == nil {
			return SubagentAttachInfo{}, false
		}
	}
	info := SubagentAttachInfo{
		NodeID:          id,
		Name:            rec.name,
		Session:         rec.session,
		ParentSessionID: rec.parentSession,
		ParentAgent:     rec.parentAgentName,
	}
	if rec.agent != nil {
		info.Agent = rec.agent.Name()
	}
	if node, ok := r.subagents.tree.Node(id); ok {
		if info.Agent == "" {
			info.Agent = node.Agent
		}
		if info.ParentAgent == "" {
			if parent, ok := r.subagents.tree.Node(node.Parent); ok {
				info.ParentAgent = parent.Agent
			}
		}
	}
	return info, true
}

// SubagentNodeForSession returns the subagent node backing a child
// sub-session, when this runtime's manager tracks one for it. Lets the TUI
// resolve session links (e.g. a nested tab's "parent" link) to an attachable
// subagent even when no tab is open for that session.
func (r *LocalRuntime) SubagentNodeForSession(sessionID string) (subagent.NodeID, bool) {
	return r.subagents.nodeForSession(sessionID)
}
