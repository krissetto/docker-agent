package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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
	closed    bool
	sessions  map[string]*sessionSubagents
	children  map[subagent.NodeID]*childRecord

	wakeOnce   sync.Once
	wakeSignal chan struct{}
	restoreMu  sync.Mutex
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

	state  subagent.NodeState
	result string
	errMsg string
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
				unwatchStarted := driver.OnStarted(func() { m.updateRootLifecycle(sess.ID, subagent.NodeRunning, "") })
				unwatchSettled := driver.OnSettled(func() {
					if lastErr := driver.LastError(); lastErr != "" {
						m.updateRootLifecycle(sess.ID, subagent.NodeFailed, lastErr)
						return
					}
					m.updateRootLifecycle(sess.ID, subagent.NodeIdle, "")
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
	if store, ok := m.r.subagentStore.(subagent.DurableStore); ok {
		full.Durability = store.Durability()
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	type sessionSnapshot struct {
		sess *session.Session
		snap subagent.Snapshot
	}
	var snapshots []sessionSnapshot
	for _, st := range m.sessions {
		if !st.topLevel || st.sess == nil {
			continue
		}
		rootID := subagent.SessionRootID(st.sess.ID)
		if snap, ok := snapshotForRoot(full, rootID); ok {
			snapshots = append(snapshots, sessionSnapshot{sess: st.sess, snap: snap})
		}
	}
	m.mu.Unlock()

	for _, item := range snapshots {
		item.sess.SetSubagentTree(&item.snap)
		if _, ok := m.r.subagentStore.(*subagent.InMemoryStore); ok {
			_ = m.r.subagentStore.SaveTree(context.WithoutCancel(m.ctx), item.sess.ID, item.snap)
			continue
		}
		if persist := m.persistence(); persist != nil {
			persist.enqueueTree(item.sess.ID, item.snap)
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

func (m *subagentManager) registerIdleChild(parentSess *session.Session, parentAgentName string, childSess *session.Session, childAgent *agent.Agent, ref subagent.AllowedSubagent) error {
	priorTree := m.tree.Snapshot()
	priorParentTree := parentSess.GetSubagentTree()
	m.mu.Lock()
	if err := m.spawnAdmissionErrorLocked(parentSess); err != nil {
		m.mu.Unlock()
		return err
	}
	parent := m.ensureSessionLocked(parentSess, parentAgentName, "")
	childID := m.tree.NewNodeID()
	if err := m.tree.Add(subagent.Node{ID: childID, Agent: ref.Agent, Name: ref.Name, Description: ref.Description, Parent: parent.node, SessionID: childSess.ID, State: subagent.NodeIdle}); err != nil {
		m.mu.Unlock()
		return err
	}
	m.ensureSessionLocked(childSess, ref.Agent, childID)
	rec := &childRecord{name: ref.DisplayName(), parentSession: parentSess.ID, parentAgentName: parentAgentName, sessionID: childSess.ID, session: childSess, parentSess: parentSess, agent: childAgent, state: subagent.NodeIdle}
	if driver, ok := m.r.sessionDrivers.Lookup(childSess.ID); ok {
		settled, started := driver.OnSettled(func() { m.reportChildSettled(childID) }), driver.OnStarted(func() { m.markChildRunning(childID) })
		rec.unwatch = func() { settled(); started() }
	}
	m.children[childID] = rec
	m.mu.Unlock()
	full := m.tree.Snapshot()
	rootID := subagent.SessionRootID(m.rootSessionLockedSafe(parentSess.ID))
	if snapshot, ok := snapshotForRoot(full, rootID); ok && m.r.subagentStore != nil {
		if err := m.r.subagentStore.SaveTree(context.WithoutCancel(m.ctx), string(rootID)[5:], snapshot); err != nil {
			if rec.unwatch != nil {
				rec.unwatch()
			}
			m.mu.Lock()
			delete(m.children, childID)
			delete(m.sessions, childSess.ID)
			_ = m.tree.Remove(childID)
			m.mu.Unlock()
			parentSess.SetSubagentTree(priorParentTree)
			if prior, ok := snapshotForRoot(priorTree, rootID); ok {
				_ = m.r.subagentStore.SaveTree(context.WithoutCancel(m.ctx), string(rootID)[5:], prior)
			}
			return err
		}
	}
	parentSess.SetSubagentTree(func() *subagent.Snapshot { snapshot, _ := snapshotForRoot(full, rootID); return &snapshot }())
	if persist := m.persistence(); persist != nil {
		if err := persist.flushNow(); err != nil {
			return err
		}
	}
	return nil
}

// Spawn starts a subagent concurrently and returns its node id immediately.
func (m *subagentManager) Spawn(parentSess *session.Session, parentAgentName string, ref subagent.AllowedSubagent, task string) (subagent.NodeID, error) {
	m.mu.Lock()
	if err := m.spawnAdmissionErrorLocked(parentSess); err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.mu.Unlock()

	childID := m.tree.NewNodeID()

	childAgent, err := m.r.team.Agent(ref.Agent)
	if err != nil {
		m.mu.Lock()
		if !m.parentAcceptsSpawnLocked(parentSess.ID) {
			m.mu.Unlock()
			return "", errors.New("parent session is stopped")
		}
		parent := m.ensureSessionLocked(parentSess, parentAgentName, "")
		_ = m.tree.Add(subagent.Node{ID: childID, Agent: ref.Agent, Name: ref.Name, Parent: parent.node, State: subagent.NodeFailed, Error: err.Error(), NeedsAttention: true, WaitingOn: "failed"})
		m.children[childID] = &childRecord{name: ref.DisplayName(), parentSession: parentSess.ID, state: subagent.NodeFailed, errMsg: err.Error()}
		m.mu.Unlock()
		m.persistSnapshot()
		m.deliverToParent(parentSess.ID, systemInfo(fmt.Sprintf("Subagent %q (%s) failed to start: %v", ref.DisplayName(), childID, err)))
		return childID, nil
	}

	toolsApproved, safetyPolicy, permissions := parentSess.SafetySettings()
	cfg := SubSessionConfig{
		AgentName:      ref.Agent,
		ToolsApproved:  toolsApproved,
		SafetyPolicy:   safetyPolicy,
		Permissions:    permissions,
		NonInteractive: true,
		PinAgent:       true,
	}
	childSess := newSubSession(parentSess, cfg, childAgent)
	childSess.AsyncSubagent = true
	// No Task in the config: async subagents are persistent sessions whose
	// "user" is their parent, so instead of the delegation boilerplate the
	// task IS the child's first regular user prompt — its session reads like
	// an ordinary session in attached tabs, read_subagent transcripts, and
	// the model's own context alike.
	childSess.AddMessage(session.UserMessage(task))
	// Like any other session, the child gets its title generated from its
	// first user message (the task) instead of a hardcoded label.

	m.mu.Lock()
	if err := m.spawnAdmissionErrorLocked(parentSess); err != nil {
		m.mu.Unlock()
		return "", err
	}
	parent := m.ensureSessionLocked(parentSess, parentAgentName, "")
	if err := m.tree.Add(subagent.Node{
		ID:          childID,
		Agent:       ref.Agent,
		Name:        ref.Name,
		Description: ref.Description,
		Parent:      parent.node,
		SessionID:   childSess.ID,
		Task:        task,
		State:       subagent.NodeStarting,
	}); err != nil {
		m.mu.Unlock()
		return "", err
	}
	// Register the child session under its own node so subagents it spawns are
	// linked beneath it (correct deep-tree linkage).
	m.ensureSessionLocked(childSess, ref.Agent, childID)
	rec := &childRecord{
		name:            ref.DisplayName(),
		parentSession:   parentSess.ID,
		parentAgentName: parentAgentName,
		sessionID:       childSess.ID,
		session:         childSess,
		parentSess:      parentSess,
		agent:           childAgent,
		state:           subagent.NodeStarting,
	}
	m.children[childID] = rec
	childDriver, err := m.r.sessionDrivers.GetInitialized(m.ctx, childSess)
	if err != nil {
		delete(m.children, childID)
		delete(m.sessions, childSess.ID)
		_ = m.tree.Remove(childID)
		m.mu.Unlock()
		return "", err
	}
	if m.r.sessionStore != nil {
		childSess.SetAttribute(SessionAgentAttribute, childSess.AgentName)
		if _, getErr := m.r.sessionStore.GetSession(m.ctx, childSess.ID); errors.Is(getErr, session.ErrNotFound) {
			if addErr := m.r.sessionStore.AddSession(m.ctx, childSess.Clone()); addErr != nil && !errors.Is(addErr, session.ErrAlreadyExists) {
				_ = m.r.sessionDrivers.ReleaseDriver(context.WithoutCancel(m.ctx), childSess.ID, childDriver)
				delete(m.children, childID)
				delete(m.sessions, childSess.ID)
				_ = m.tree.Remove(childID)
				m.mu.Unlock()
				return "", addErr
			}
		} else if getErr != nil {
			_ = m.r.sessionDrivers.ReleaseDriver(context.WithoutCancel(m.ctx), childSess.ID, childDriver)
			delete(m.children, childID)
			delete(m.sessions, childSess.ID)
			_ = m.tree.Remove(childID)
			m.mu.Unlock()
			return "", getErr
		}
	}
	childDriver.SetPreStartErrorGate(func() error { return m.admitChildRun(childID) }, func() { m.abortChildStart(childID) })
	rec.unwatch = childDriver.OnSettled(func() { m.reportChildSettled(childID) })
	unwatchStarted := childDriver.OnStarted(func() { m.markChildRunning(childID) })
	previousUnwatch := rec.unwatch
	rec.unwatch = func() {
		previousUnwatch()
		unwatchStarted()
	}
	// Register accepted work before releasing m.mu, the lock Close uses to
	// close admission. This guarantees Close cannot observe an empty group and
	// return before a committed spawn starts its work.
	if m.r.TitleGenerator(m.ctx) != nil {
		m.wg.Go(func() { m.startChildTitle(childSess, task) })
	}
	m.wg.Go(func() {
		for range childDriver.Drive(m.ctx, childSess) {
		}
	})
	m.wg.Add(1) // covers the synchronous post-commit persistence below
	m.mu.Unlock()

	defer m.wg.Done()
	m.persistSnapshot()
	if persist := m.persistence(); persist != nil {
		if err := persist.flushNow(); err != nil {
			return "", err
		}
	}

	return childID, nil
}

type childAdmissionDenial string

const (
	childAdmissionParentStopped childAdmissionDenial = "parent_stopped"
	childAdmissionDepth         childAdmissionDenial = "depth"
	childAdmissionGlobalActive  childAdmissionDenial = "global_active"
	childAdmissionRootActive    childAdmissionDenial = "root_active"
	childAdmissionMissing       childAdmissionDenial = "missing"
	childAdmissionStopped       childAdmissionDenial = "stopped"
	childAdmissionRunning       childAdmissionDenial = "running"
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
		if rec.state == subagent.NodeRunning {
			return childAdmissionDecision{denial: childAdmissionRunning, parentSession: parentSession}
		}
	}

	active, rootActive := 0, 0
	rootID := m.rootSessionLocked(parentSession)
	for _, rec := range m.children {
		if !activeSubagentState(rec.state) {
			continue
		}
		active++
		if m.rootSessionLocked(rec.parentSession) == rootID {
			rootActive++
		}
	}
	if !limitAllows(active, m.r.maxActiveDescendants) {
		return childAdmissionDecision{denial: childAdmissionGlobalActive, parentSession: parentSession, limit: m.r.maxActiveDescendants}
	}
	if !limitAllows(rootActive, m.r.maxActiveDescendantsRoot) {
		return childAdmissionDecision{denial: childAdmissionRootActive, parentSession: parentSession, limit: m.r.maxActiveDescendantsRoot}
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
	case childAdmissionGlobalActive:
		return &SessionError{Kind: SessionErrorCapacity, SessionID: decision.parentSession, Operation: "active_descendants", Limit: decision.limit}
	case childAdmissionRootActive:
		return &SessionError{Kind: SessionErrorCapacity, SessionID: decision.parentSession, Operation: "active_descendants_root", Limit: decision.limit}
	case childAdmissionMissing:
		return &SessionError{Kind: SessionErrorNotFound, Operation: "admit_child_run"}
	case childAdmissionStopped:
		sessionID := ""
		if rec := m.children[request.childID]; rec != nil {
			sessionID = rec.sessionID
		}
		return &SessionError{Kind: SessionErrorStopped, SessionID: sessionID, Operation: "admit_child_run"}
	case childAdmissionRunning:
		sessionID := ""
		if rec := m.children[request.childID]; rec != nil {
			sessionID = rec.sessionID
		}
		return &SessionError{Kind: SessionErrorInvalid, SessionID: sessionID, Operation: "admit_child_running"}
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
	decision := m.childAdmissionLocked(request)
	if err := m.childAdmissionError(request, decision); err != nil {
		return err
	}
	rec := m.children[childID]
	if rec.state == subagent.NodeStarting {
		return nil // Spawn committed this reservation before starting its driver.
	}
	rec.state = subagent.NodeStarting
	_ = m.tree.Update(childID, func(n *subagent.Node) { n.State = subagent.NodeStarting })
	return nil
}

func (m *subagentManager) abortChildStart(childID subagent.NodeID) {
	m.mu.Lock()
	rec := m.children[childID]
	if rec == nil || rec.state != subagent.NodeStarting {
		m.mu.Unlock()
		return
	}
	rec.state = subagent.NodeIdle
	_ = m.tree.Update(childID, func(n *subagent.Node) { n.State = subagent.NodeIdle })
	m.mu.Unlock()
	m.signalCapacityRelease()
}

// signalCapacityRelease coalesces admission opportunities. The worker is
// started lazily so focused tests that build a manager literal get the same
// lifecycle behavior as production constructors.
func (m *subagentManager) signalCapacityRelease() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.wakeOnce.Do(func() {
		m.wakeSignal = make(chan struct{}, 1)
		m.wg.Go(m.capacityReleaseWorker)
	})
	wake := m.wakeSignal
	m.mu.Unlock()
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (m *subagentManager) capacityReleaseWorker() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.wakeSignal:
			m.mu.Lock()
			type candidate struct {
				id        subagent.NodeID
				sessionID string
			}
			candidates := make([]candidate, 0, len(m.children))
			for id, rec := range m.children {
				if rec != nil && rec.state != subagent.NodeStopped && !activeSubagentState(rec.state) {
					candidates = append(candidates, candidate{id: id, sessionID: rec.sessionID})
				}
			}
			m.mu.Unlock()
			slices.SortFunc(candidates, func(a, b candidate) int { return strings.Compare(a.sessionID, b.sessionID) })

			// Driver locks are intentionally acquired only after releasing m.mu;
			// WakePending's gate may call back into the manager.
			for _, candidate := range candidates {
				d, ok := m.r.sessionDrivers.Lookup(candidate.sessionID)
				if ok && d.HasPending() {
					d.WakePending()
				}
			}
		}
	}
}

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
	if rec == nil || rec.state != subagent.NodeStarting {
		m.mu.Unlock()
		return
	}
	rec.state = subagent.NodeRunning
	_ = m.tree.Update(childID, func(n *subagent.Node) { n.State = subagent.NodeRunning })
	m.mu.Unlock()
	m.persistSnapshot()
}

// reportChildSettled records a child driver's finished turn and notifies its
// parent when the child has no pending input left. The child remains alive and
// re-messageable until explicitly stopped.
func (m *subagentManager) reportChildSettled(childID subagent.NodeID) {
	m.mu.Lock()
	rec := m.children[childID]
	// Only the currently active turn may settle. Stop and any newer lifecycle
	// transition win permanently over a delayed callback.
	if rec == nil || rec.state != subagent.NodeRunning {
		m.mu.Unlock()
		return
	}
	// Driver state has its own lock. Snapshot identity, release the manager
	// lock, then revalidate before committing the completion so stop/new-run
	// transitions cannot be overwritten.
	sessionID := rec.sessionID
	m.mu.Unlock()
	errMsg := ""
	if d, ok := m.r.sessionDrivers.Lookup(sessionID); ok {
		errMsg = d.LastError()
	}

	m.mu.Lock()
	rec = m.children[childID]
	if rec == nil || rec.state != subagent.NodeRunning || rec.sessionID != sessionID {
		m.mu.Unlock()
		return
	}
	rec.result = rec.session.GetLastAssistantMessageContent()
	rec.errMsg = errMsg
	parentSess, childSess, parentAgentName, childAgent := rec.parentSess, rec.session, rec.parentAgentName, rec.agent
	state := subagent.NodeIdle
	if rec.errMsg != "" {
		state = subagent.NodeFailed
	}
	rec.state = state
	_ = m.tree.Update(childID, func(n *subagent.Node) {
		n.State = state
		n.Error = rec.errMsg
		n.NeedsAttention = state == subagent.NodeFailed
		if n.NeedsAttention {
			n.WaitingOn = "failed"
		} else {
			n.WaitingOn = ""
		}
	})
	errMsg = rec.errMsg
	m.mu.Unlock()
	m.signalCapacityRelease()

	if parentSess != nil && childSess != nil {
		parentSess.AddSubSession(childSess)
		if persist := m.persistence(); persist != nil {
			persist.enqueueTranscript(parentSess.ID, childSess)
		}
	}
	if parentSess != nil && childSess != nil && childAgent != nil {
		parentAgent, _ := m.r.team.Agent(parentAgentName)
		m.r.executeSubagentStopHooks(context.WithoutCancel(m.ctx), parentSess, childSess, parentAgent, childAgent.Name(), childSess.GetLastAssistantMessageContent())
	}

	m.reportTurn(childID, state, errMsg)
}

// reportTurn records a child's finished turn in the tree and delivers a short
// report once its subtree is quiet. The child stays alive and idle, accepting
// future input until explicitly stopped.
func (m *subagentManager) reportTurn(childID subagent.NodeID, state subagent.NodeState, errMsg string) {
	m.mu.Lock()
	rec := m.children[childID]
	// Hooks and persistence may have allowed a stop or a new admitted run while
	// this report was being prepared. Never publish that stale completion.
	if rec == nil || rec.state != state {
		m.mu.Unlock()
		return
	}
	parentID, name := rec.parentSession, rec.name
	childSessionID := rec.sessionID
	detail := rec.result
	if errMsg != "" {
		detail = errMsg
	}
	_ = m.tree.Update(childID, func(n *subagent.Node) {
		n.State = state
		n.Error = errMsg
	})
	m.mu.Unlock()

	m.persistSnapshot()

	// Wait for quiet: a turn that ends while the subagent's own subtree is
	// still working is bookkeeping, not news — the parent can act on nothing
	// yet, and in deep chains reporting it would wake every ancestor once per
	// leaf event (a model turn each). Stay silent; the subagent is re-woken by
	// its own children's reports, and its next turn end re-evaluates this
	// check, so a report always bubbles up once the subtree settles.
	// send_message(parent) never waits.
	if m.hasRunningSubagents(childSessionID) {
		return
	}

	verb := "finished its turn"
	label := "Full response"
	if state == subagent.NodeFailed {
		verb = "failed"
		label = "Error"
	}
	preview, truncated := subagent.PreviewText(detail, subagent.PreviewLen)
	msg := fmt.Sprintf("Subagent %q (%s) %s.", name, childID, verb)
	// Embed the response (or its head) so the parent can often decide without
	// a read_subagent round-trip. A trailing "[...]" marks truncation: the
	// full text requires inspection. Without it, the report carries the
	// ENTIRE response.
	if preview != "" {
		if truncated {
			msg += fmt.Sprintf(" %s preview: %q", label, preview+" [...]")
		} else {
			msg += fmt.Sprintf(" %s: %q", label, preview)
		}
	}
	m.deliverToParent(parentID, systemInfo(msg))
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
		if stoppedRec.parentSess != nil && stoppedRec.session != nil {
			stoppedRec.parentSess.AddSubSession(stoppedRec.session)
		}
		if stoppedRec.parentSession != "" && stoppedRec.session != nil {
			if persist := m.persistence(); persist != nil {
				persist.enqueueTranscript(stoppedRec.parentSession, stoppedRec.session)
			}
		}
	}

	m.persistSnapshot()
	if persist := m.persistence(); persist != nil {
		if err := persist.flushNow(); err != nil {
			return "", err
		}
	}
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
	return m.r.sessionDrivers.PostKnown(m.ctx, sessionID, QueuedMessage{Content: content}, true)
}

// deliverToParent routes a runtime-authored note (turn report, child
// message, spawn failure) up to a parent session. Notes must never be lost:
// when the parent driver has not been seen yet they remain buffered until the
// session appears.
func (m *subagentManager) deliverToParent(sessionID, content string) {
	if !m.r.deliverOrBuffer(m.ctx, sessionID, content) {
		slog.WarnContext(m.ctx, "Dropped runtime lifecycle note: session mailbox full or stopped", "session_id", sessionID)
	}
}

// deliverExplicitToParent preserves child-to-top-level delivery when the
// parent's driver has not been observed yet, but otherwise uses direct
// admission semantics so capacity denial is surfaced to the tool caller.
func (m *subagentManager) deliverExplicitToParent(sessionID, content string) bool {
	return m.r.sessionDrivers.PostOrBuffer(m.ctx, sessionID, QueuedMessage{Content: content}, true)
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
	driver, err := m.r.sessionDrivers.GetInitialized(ctx, loaded)
	if err != nil {
		return fail(err)
	}
	driver.SetPreStartErrorGate(func() error { return m.admitChildRun(id) }, func() { m.abortChildStart(id) })
	unwatchSettled := driver.OnSettled(func() { m.reportChildSettled(id) })
	unwatchStarted := driver.OnStarted(func() { m.markChildRunning(id) })
	unwatch := func() { unwatchSettled(); unwatchStarted() }

	m.mu.Lock()
	current := m.children[id]
	if m.closed || current == nil || current.state == subagent.NodeStopped {
		m.mu.Unlock()
		unwatch()
		_ = m.r.sessionDrivers.ReleaseDriver(context.WithoutCancel(ctx), sessionID, driver)
		return fmt.Errorf("subagent %q is no longer available", id)
	}
	oldUnwatch := current.unwatch
	current.session = loaded
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
	if !m.deliver(sessionID, body) {
		// Capacity reclamation can race the first lookup. Rebind once more and
		// retry rather than reporting a persistent child as permanently gone.
		if err := m.ensureChildDriver(m.ctx, id); err != nil || !m.deliver(sessionID, body) {
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
		if rec.state == subagent.NodeRunning || rec.state == subagent.NodeStarting {
			return true
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
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.children[id]
	if rec == nil {
		return childRecord{}, fmt.Errorf("no subagent with id %q", id)
	}
	if rec.parentSession != parentID {
		return childRecord{}, fmt.Errorf("subagent %q is not one of yours", id)
	}
	return *rec, nil
}

// Read returns a snapshot of a subagent's record by id.
func (m *subagentManager) Read(id subagent.NodeID) (childRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.children[id]
	if !ok {
		return childRecord{}, false
	}
	return *rec, true
}

func (m *subagentManager) deleteSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	var stopped []*childRecord
	for id, rec := range m.children {
		if rec.parentSession == sessionID && rec.state != subagent.NodeStopped {
			stopped = m.markStoppedLocked(rec, id, stopped)
		}
	}
	delete(m.sessions, sessionID)
	for _, rec := range stopped {
		delete(m.sessions, rec.sessionID)
	}
	m.mu.Unlock()
	for _, rec := range stopped {
		if rec.unwatch != nil {
			rec.unwatch()
		}
		m.r.sessionEvents.Delete(rec.sessionID)
	}
	for _, rec := range stopped {
		m.r.sessionDrivers.StopAll(rec.sessionID)
	}
	for _, rec := range stopped {
		if err := m.r.sessionDrivers.Delete(ctx, rec.sessionID); err != nil {
			return err
		}
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
func (m *subagentManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	for _, tracked := range m.sessions {
		if tracked.unwatch != nil {
			tracked.unwatch()
			tracked.unwatch = nil
		}
	}
	m.mu.Unlock()

	// Restore owns this gate from admission through its final persistence.
	// Marking closed first rejects new restores; acquiring it waits for any
	// admitted restore before workers and persistence are shut down.
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	m.cancel()
	m.wg.Wait()
	m.persistMu.Lock()
	persist := m.persist
	m.persistMu.Unlock()
	if persist != nil {
		if err := persist.close(); err != nil {
			slog.Warn("Failed to flush subagent persistence", "error", err)
		}
	}
}

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
	if !ok || rec.session == nil {
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
