package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"weak"

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
	wg     *driverWorkGroup

	mu                sync.Mutex
	metricsMu         sync.Mutex
	persistMu         sync.Mutex
	persist           *subagentPersistence
	coord             session.CoordinationStore
	closed            bool
	stopping          map[string]bool
	stoppedRoots      map[string]bool
	stoppedAdmissions map[string]bool
	stopFence         atomic.Pointer[map[string]bool]
	pendingStops      map[subagent.NodeID][]session.ChildCommit
	sessions          map[string]*sessionSubagents
	children          map[subagent.NodeID]*childRecord

	transitionMu sync.Mutex
	transitions  map[string]*lifecycleTransition
	admissions   map[string]session.ChildRecord
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
	driver   weak.Pointer[sessionDriver]
}

// childRecord retains what read_subagent / send_message / reporting need about
// a subagent. The sessionDriver owns run/wake/cancel state; unwatch releases
// the manager's completion callback when the child is stopped.
type childRecord struct {
	synchronous     bool
	name            string
	parentSession   string
	parentAgentName string
	sessionID       string
	session         *session.Session
	parentSess      *session.Session
	agent           *agent.Agent
	unwatch         func()

	durable session.ChildRecord
}

// childRead is an ephemeral read projection, never retained by the manager.
type childRead struct {
	childRecord

	state  subagent.NodeState
	result string
	errMsg string
}

func newSubagentManager(r *LocalRuntime) *subagentManager {
	ctx, cancel := context.WithCancel(r.ctx())
	return &subagentManager{
		wg:       newDriverWorkGroup(),
		r:        r,
		tree:     subagent.NewTree(),
		ctx:      ctx,
		cancel:   cancel,
		sessions: map[string]*sessionSubagents{},
		children: map[subagent.NodeID]*childRecord{},
	}
}

type lifecycleTransition struct{ token chan struct{} }

func (t *lifecycleTransition) LockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case t.token <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (t *lifecycleTransition) Lock()   { t.token <- struct{}{} }
func (t *lifecycleTransition) Unlock() { <-t.token }

func (m *subagentManager) transition(root string) *lifecycleTransition {
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	if m.transitions == nil {
		m.transitions = make(map[string]*lifecycleTransition)
	}
	lock := m.transitions[root]
	if lock == nil {
		lock = &lifecycleTransition{token: make(chan struct{}, 1)}
		m.transitions[root] = lock
	}
	return lock
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
	// older concurrent root snapshot can be persisted after a newer one.
	m.metricsMu.Lock()
	defer m.metricsMu.Unlock()
	_, _ = m.tree.UpdateIfChanged(id, func(node *subagent.Node) {
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
		if st.topLevel && m.r.sessionDrivers != nil && !m.closed {
			if driver, ok := m.r.sessionDrivers.Lookup(sess.ID); ok {
				m.bindRootDriverLocked(sess.ID, st, driver)
			}
		}
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
		if m.r.sessionDrivers != nil && !m.closed {
			if driver, ok := m.r.sessionDrivers.Lookup(sess.ID); ok {
				m.bindRootDriverLocked(sess.ID, st, driver)
			}
		}
	}
	m.sessions[sess.ID] = st
	return st
}

// bindPublishedRootDriver reconnects an already tracked root to a published
// driver without making mere session creation a topology mutation. Registry
// callers must release their lock before calling this, and bind before waking
// accepted input so restored roots observe the first resumed turn.
func (m *subagentManager) bindPublishedRootDriver(driver *sessionDriver) {
	if driver == nil || driver.identityParent != "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if tracked := m.sessions[driver.identityID]; tracked != nil && tracked.topLevel {
		m.bindRootDriverLocked(driver.identityID, tracked, driver)
	}
}

// bindRootDriverLocked binds projection callbacks to one driver generation,
// independently of topology registration. Caller holds m.mu. Weak references
// let idle registry eviction reclaim the driver and its transcript.
func (m *subagentManager) bindRootDriverLocked(sessionID string, st *sessionSubagents, driver *sessionDriver) {
	ref := weak.Make(driver)
	if m.closed || !st.topLevel || st.driver == ref {
		return
	}
	if st.unwatch != nil {
		st.unwatch()
	}
	st.sess = nil
	st.driver = ref
	unwatchStarted := driver.OnStarted(func() {
		m.updateRootLifecycle(sessionID, ref, subagent.NodeRunning, "")
	})
	unwatchSettled := driver.OnSettled(func() {
		current := ref.Value()
		if current == nil {
			return
		}
		if lastErr := current.LastError(); lastErr != "" {
			m.updateRootLifecycle(sessionID, ref, subagent.NodeFailed, lastErr)
			return
		}
		m.updateRootLifecycle(sessionID, ref, subagent.NodeIdle, "")
	})
	st.unwatch = func() { unwatchStarted(); unwatchSettled() }
}

// persistSnapshot mirrors dirty subtrees onto live session owners and stores
// each top-level root's swarm without leaking unrelated session trees.
func (m *subagentManager) persistSnapshot() {
	m.persistSnapshotLocked(false)
}

func (m *subagentManager) persistSnapshotLocked(alreadyLocked bool) {
	if !alreadyLocked {
		m.metricsMu.Lock()
		defer m.metricsMu.Unlock()
	}
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
	for _, snap := range m.tree.TakeDirtySnapshots() {
		id, ok := strings.CutPrefix(string(snap.Root), "root:")
		if !ok {
			continue
		}
		tracked := m.sessions[id]
		if tracked == nil || !tracked.topLevel {
			continue
		}
		snap.Durability = m.r.sessionDurability()
		projections = append(projections, projection{id, tracked.sess, snap})
	}
	m.mu.Unlock()
	for _, item := range projections {
		if m.r.sessionDrivers != nil {
			for _, id := range subtreeSessionIDs(item.snap.Nodes[0]) {
				d, ok := m.r.sessionDrivers.Lookup(id)
				if !ok {
					continue
				}
				node, _ := subtreeForSession(item.snap, id)
				tree := subagent.Snapshot{Version: item.snap.Version, Durability: item.snap.Durability, Root: node.Node.ID, Nodes: []subagent.NodeSnapshot{node}}
				_ = d.ownerCall(m.r.lifetime(), func() error {
					d.sess.SetSubagentTree(&tree)
					d.events.Publish(d.identityID, SubagentTree(tree))
					return nil
				})
				if id == item.id {
					item.sess = nil
				}
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

func (m *subagentManager) updateRootLifecycle(sessionID string, driver weak.Pointer[sessionDriver], state subagent.NodeState, errMsg string) {
	m.mu.Lock()
	tracked := m.sessions[sessionID]
	if tracked == nil || !tracked.topLevel || tracked.driver != driver || m.closed {
		m.mu.Unlock()
		return
	}
	nodeID := tracked.node
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
	m.mu.Unlock()
	m.persistSnapshot()
}

func (m *subagentManager) registerIdleChild(parent *session.Session, parentAgent string, child *session.Session, target *agent.Agent, ref subagent.AllowedSubagent) error {
	_, err := m.admitChild(parent, parentAgent, child, target, ref, "", false)
	return err
}

func (m *subagentManager) Spawn(parent *session.Session, parentAgent string, ref subagent.AllowedSubagent, task string) (subagent.NodeID, error) {
	if !m.r.sessionDelegationEnabled(parent) {
		return "", errSubagentsDisabled
	}
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
	child.DelegationLineage = append(parent.DelegationLineageSnapshot(), parentAgent)
	turnID, err := newSessionRequestID()
	if err != nil {
		return "", err
	}
	input := session.UserMessage(task)
	input.Pending, input.Accepted, input.TurnID = true, true, turnID
	input.InputOrigin, input.SenderID, input.SenderName, input.InputMode = session.InputOriginAgent, parent.ID, parentAgent, "turn"
	child.AddMessage(input)
	id, err := m.admitChild(parent, parentAgent, child, target, ref, task, true)
	if err != nil {
		return "", err
	}
	if d, ok := m.r.sessionDrivers.Lookup(child.ID); ok {
		d.WakePending()
	}
	return id, nil
}

func (m *subagentManager) admitChild(parent *session.Session, parentAgent string, child *session.Session, target *agent.Agent, ref subagent.AllowedSubagent, task string, autonomous bool) (subagent.NodeID, error) {
	return m.admitChildContext(m.ctx, parent, parentAgent, child, target, ref, task, autonomous)
}

func (m *subagentManager) admitChildContext(ctx context.Context, parent *session.Session, parentAgent string, child *session.Session, target *agent.Agent, ref subagent.AllowedSubagent, task string, autonomous bool) (subagent.NodeID, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
	defer cancel()
	transition := m.transition(m.rootSessionLockedSafe(parent.ID))
	if err := transition.LockContext(ctx); err != nil {
		return "", err
	}
	defer transition.Unlock()
	if autonomous && !m.r.acceptSessionDelegation(parent) {
		return "", errSubagentsDisabled
	}
	if m.r.sessionStore != nil {
		if _, err := m.r.sessionStore.GetSession(ctx, parent.ID); errors.Is(err, session.ErrNotFound) {
			parent.SetAttribute(SessionAgentAttribute, parentAgent)
			if err := m.r.sessionStore.AddSession(ctx, parent.OwnSnapshot()); err != nil && !errors.Is(err, session.ErrAlreadyExists) {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
	}
	child.SetAttribute(SessionAgentAttribute, ref.Agent)
	child.SetAttribute(SessionParentAgentAttribute, parentAgent)
	reservation, err := m.r.sessionDrivers.PrepareRestore(ctx, child)
	if err != nil {
		return "", err
	}
	defer reservation.Discard()
	if task != "" || autonomous {
		if err := m.r.sessionDrivers.admitRun(reservation.driver); err != nil {
			return "", err
		}
	}
	m.mu.Lock()
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return "", err
	}
	if err := m.spawnAdmissionErrorLocked(parent); err != nil {
		m.mu.Unlock()
		return "", err
	}
	tracked := m.ensureSessionLocked(parent, parentAgent, "")
	id := m.tree.NewNodeID()
	node := subagent.Node{ID: id, Parent: tracked.node, SessionID: child.ID, Agent: ref.Agent, Name: ref.Name, Description: ref.Description, State: subagent.NodeIdle, CreatedAt: time.Now()}
	node.Task, _ = subagent.PreviewText(task, subagent.PreviewLen)
	record := session.ChildRecord{RootSessionID: m.rootSessionLocked(parent.ID), ParentSessionID: parent.ID, Node: node, Revision: 1}
	rec := &childRecord{name: ref.DisplayName(), parentSession: parent.ID, parentAgentName: parentAgent, sessionID: child.ID, agent: target, durable: record}
	if _, resident := m.r.sessionDrivers.Lookup(parent.ID); !resident {
		rec.parentSess = parent.Clone()
	}
	d := reservation.driver
	d.SetPreStartErrorGate(func() error { return m.admitChildRun(id) }, nil)
	rec.unwatch = d.OnStarted(func() { m.markChildRunning(id) })
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		rec.unwatch()
		return "", err
	}
	if m.admissions == nil {
		m.admissions = make(map[string]session.ChildRecord)
	}
	m.admissions[child.ID] = record
	m.publishStopFenceLocked()
	m.mu.Unlock()
	err = m.admitDurableChild(parent, child, record)
	if err != nil {
		rec.unwatch()
		return "", err
	}
	m.mu.Lock()
	fenced := !m.parentAcceptsSpawnLocked(parent.ID)
	if fenced {
		m.mu.Unlock()
		stopped := record
		stopped.Node.State = subagent.NodeStopped
		if err := m.commitChildren(m.ctx, []session.ChildCommit{{ExpectedRevision: record.Revision, Record: stopped}}); err != nil {
			rec.unwatch()
			return "", err
		}
		m.mu.Lock()
		delete(m.admissions, child.ID)
		if m.stoppedAdmissions == nil {
			m.stoppedAdmissions = make(map[string]bool)
		}
		m.stoppedAdmissions[child.ID] = true
		m.publishStopFenceLocked()
		m.mu.Unlock()
		rec.unwatch()
		return "", &SessionError{Kind: SessionErrorStopped, SessionID: child.ID, Operation: "admit_child"}
	}
	delete(m.admissions, child.ID)
	err = m.r.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, func() error {
		if err := m.tree.Add(node); err != nil {
			return err
		}
		m.children[id] = rec
		m.sessions[child.ID] = &sessionSubagents{node: id}
		return nil
	})
	// Admission and registry activation have committed. Publish once to the
	// parent's existing journal; restore/import never passes through this path.
	if err == nil {
		events := m.r.sessionEvents
		if parentDriver, ok := m.r.sessionDrivers.Lookup(parent.ID); ok {
			events = parentDriver.events
		}
		events.Publish(parent.ID, &SubagentCreatedEvent{
			AgentContext: newAgentContext(parentAgent),
			Type:         "subagent_created", SessionID: parent.ID, ParentSessionID: parent.ID,
			ChildSessionID: child.ID, NodeID: id, CreatedAt: node.CreatedAt,
		})
	}
	if err == nil && task != "" && m.r.TitleGenerator(m.ctx) != nil {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.startChildTitle(child, task)
		}()
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
		if rec.durable.Node.State == subagent.NodeStopped || m.closed || m.sessionStoppingLocked(rec.sessionID) || !m.parentAcceptsSpawnLocked(rec.durable.RootSessionID) {
			return childAdmissionDecision{denial: childAdmissionStopped, parentSession: parentSession}
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

// sessionAdmissionError is lock-free: actor admission may hold driver.mu.
func (m *subagentManager) sessionAdmissionError(sessionID string) error {
	if fence := m.stopFence.Load(); fence != nil && (*fence)[sessionID] {
		return &SessionError{Kind: SessionErrorStopped, SessionID: sessionID, Operation: "stop_subtree", Detail: "subtree stop is awaiting authoritative acknowledgement"}
	}
	return nil
}

func (m *subagentManager) publishStopFenceLocked() {
	fence := make(map[string]bool, len(m.stopping))
	maps.Copy(fence, m.stopping)
	maps.Copy(fence, m.stoppedRoots)
	maps.Copy(fence, m.stoppedAdmissions)
	for _, rec := range m.children {
		if m.stoppedRoots[rec.durable.RootSessionID] {
			fence[rec.sessionID] = true
		}
	}
	for id, rec := range m.admissions {
		if m.stoppedRoots[rec.RootSessionID] {
			fence[id] = true
		}
	}
	m.stopFence.Store(&fence)
}

func (m *subagentManager) sessionStoppingLocked(sessionID string) bool {
	seen := map[string]bool{}
	for sessionID != "" && !seen[sessionID] {
		if m.stopping[sessionID] {
			return true
		}
		seen[sessionID] = true
		parent := ""
		for _, rec := range m.children {
			if rec.sessionID == sessionID {
				parent = rec.parentSession
				break
			}
		}
		sessionID = parent
	}
	return false
}

func (m *subagentManager) parentAcceptsSpawnLocked(sessionID string) bool {
	if m.closed || m.sessionStoppingLocked(sessionID) {
		return false
	}
	if m.r.sessionDrivers != nil {
		m.r.sessionDrivers.mu.Lock()
		_, deleted := m.r.sessionDrivers.deleted[sessionID]
		_, stopped := m.r.sessionDrivers.stoppedTrees[sessionID]
		m.r.sessionDrivers.mu.Unlock()
		if deleted || stopped {
			return false
		}
	}
	for _, rec := range m.children {
		if rec.sessionID == sessionID {
			return rec.durable.Node.State != subagent.NodeStopped
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
	if rec == nil || rec.durable.Node.State == subagent.NodeStopped {
		m.mu.Unlock()
		return
	}
	_ = m.tree.Update(childID, func(n *subagent.Node) { n.State = subagent.NodeRunning })
	m.mu.Unlock()
	m.persistSnapshot()
}

// stopChild explicitly finalizes a subagent of parentID: it cancels any
// in-flight run, releases callbacks, and stops its own subagents recursively.
// Stopped subagents keep their record so read_subagent still works, but accept
// no future input.
func (m *subagentManager) stopChild(parentID string, id subagent.NodeID) (string, error) {
	return m.stopChildAuthorized(m.ctx, parentID, id, true)
}

func (m *subagentManager) stopChildContext(ctx context.Context, parentID string, id subagent.NodeID) (string, error) {
	return m.stopChildAuthorized(ctx, parentID, id, false)
}

func (m *subagentManager) stopChildAuthorized(ctx context.Context, parentID string, id subagent.NodeID, directChild bool) (string, error) {
	// The manager gate serializes child admission with the canonical stop.
	// Starting drivers recheck that gate after reserving capacity, so durable
	// I/O must not hold the registry-wide run admission mutex.
	transition := m.transition(m.rootSessionLockedSafe(parentID))
	if err := transition.LockContext(ctx); err != nil {
		return "", err
	}
	m.mu.Lock()
	rec := m.children[id]
	if rec == nil || (directChild && rec.parentSession != parentID) || m.rootSessionLocked(rec.sessionID) != m.rootSessionLocked(parentID) {
		m.mu.Unlock()
		transition.Unlock()
		return "", fmt.Errorf("no owned subagent with id %q", id)
	}
	name := rec.name
	var unwatches []func()
	var stopped []*childRecord
	var collect func(*childRecord)
	collect = func(rec *childRecord) {
		stopped = append(stopped, rec)
		for _, child := range m.children {
			if child.parentSession == rec.sessionID {
				collect(child)
			}
		}
	}
	collect(rec)
	if _, retry := m.pendingStops[id]; !retry {
		for _, child := range stopped {
			if m.stopping[child.sessionID] {
				m.mu.Unlock()
				transition.Unlock()
				return "", &SessionError{Kind: SessionErrorPersistence, SessionID: child.sessionID, Operation: "stop_subtree", Detail: "retry the unresolved subtree stop before stopping an overlapping subtree"}
			}
		}
	}
	if m.stopping == nil {
		m.stopping = make(map[string]bool)
	}
	for _, child := range stopped {
		m.stopping[child.sessionID] = true
	}
	m.publishStopFenceLocked()
	commits := make([]session.ChildCommit, 0, len(stopped))
	for _, child := range stopped {
		if child.durable.Node.State == subagent.NodeStopped {
			// Repeated stop fences a prepared or published manual-view capability.
			g := m.r.sessionDrivers
			g.mu.Lock()
			hasView := g.reservations[child.sessionID] != nil
			if driver := g.drivers[child.sessionID]; driver != nil {
				driver.mu.Lock()
				hasView = hasView || driver.stoppedView
				driver.mu.Unlock()
			}
			g.mu.Unlock()
			if !hasView {
				continue
			}
		}
		record := child.durable
		record.Node.State, record.Node.NeedsAttention, record.Node.WaitingOn = subagent.NodeStopped, false, ""
		commits = append(commits, session.ChildCommit{ExpectedRevision: record.Revision, Record: record})
	}
	if pending, ok := m.pendingStops[id]; ok {
		commits = pending
	}
	m.mu.Unlock()
	err := m.commitChildren(ctx, commits)
	m.mu.Lock()
	if err != nil {
		if m.pendingStops == nil {
			m.pendingStops = make(map[subagent.NodeID][]session.ChildCommit)
		}
		m.pendingStops[id] = commits
		m.mu.Unlock()
		transition.Unlock()
		return "", err
	}
	delete(m.pendingStops, id)
	for _, commit := range commits {
		record := commit.Record
		record.Revision++
		for _, child := range stopped {
			if child.sessionID == record.Node.SessionID {
				child.durable = record
			}
		}
		_ = m.tree.Update(record.Node.ID, func(n *subagent.Node) { n.State, n.NeedsAttention, n.WaitingOn = subagent.NodeStopped, false, "" })
	}
	for _, child := range stopped {
		if child.unwatch != nil {
			unwatches = append(unwatches, child.unwatch)
			child.unwatch = nil
		}
	}
	m.mu.Unlock()
	transition.Unlock()
	// The committed tombstone closes admission before execution is retired.
	for _, unwatch := range unwatches {
		unwatch()
	}
	for _, child := range stopped {
		m.r.sessionDrivers.StopAll(child.sessionID)
	}
	m.mu.Lock()
	for _, child := range stopped {
		delete(m.stopping, child.sessionID)
	}
	m.publishStopFenceLocked()
	m.mu.Unlock()
	for _, child := range stopped {
		d, ok := m.r.sessionDrivers.Lookup(child.sessionID)
		if !ok {
			continue
		}
		select {
		case <-d.Done():
		case <-ctx.Done():
			return "", ctx.Err()
		}
		d.mu.Lock()
		settling, generation, runErr := d.settling(), d.generation, d.completionRunErr
		d.mu.Unlock()
		if settling {
			d.finishRunContext(ctx, generation, runErr)
		}
		d.mu.Lock()
		err := d.completionErr
		d.mu.Unlock()
		if err != nil {
			return "", err
		}
	}
	m.signalCapacityRelease()
	m.persistSnapshot()
	return name, nil
}

// commitChildren requires atomic storage for subtree changes. An ambiguous
// acknowledgement is reconciled against every stable identity and revision.
func (m *subagentManager) commitChildren(ctx context.Context, commits []session.ChildCommit) error {
	if len(commits) == 0 {
		return nil
	}
	store, ok := m.coordination().(session.ChildCommitBatchStore)
	if !ok {
		return &SessionError{Kind: SessionErrorUnsupported, Operation: "stop_subagent"}
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
	defer cancel()
	err := store.CommitChildren(ctx, commits)
	if err == nil {
		return nil
	}
	reconcileCtx, reconcileCancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
	defer reconcileCancel()
	records, loadErr := m.coordination().LoadChildren(reconcileCtx, commits[0].Record.RootSessionID)
	if loadErr != nil {
		return err
	}
	for _, commit := range commits {
		matched := false
		for _, record := range records {
			if record.Node.ID == commit.Record.Node.ID && record.Node.SessionID == commit.Record.Node.SessionID && record.Revision == commit.ExpectedRevision+1 && record.Node.State == subagent.NodeStopped {
				matched = true
				break
			}
		}
		if !matched {
			return err
		}
	}
	return nil
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

// deliverExplicitToParent rejects absent inboxes rather than acknowledging a
// process-local orphan buffer as delivery.
func (m *subagentManager) deliverExplicitToParent(sessionID, content string, sender ...string) bool {
	var senderID, senderName string
	if len(sender) == 2 {
		senderID, senderName = sender[0], sender[1]
	}
	id, err := newSessionRequestID()
	if err != nil {
		return false
	}
	_, err = m.postAgentCommunication(m.ctx, sessionID, agentCommunication(content, id, senderID, senderName, subagent.DeliveryGuidance))
	return err == nil
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
	restore := m.transition("restore:" + string(id))
	restore.Lock()
	defer restore.Unlock()

	m.mu.Lock()
	rec := m.children[id]
	if rec == nil {
		m.mu.Unlock()
		return fmt.Errorf("no subagent with id %q", id)
	}
	if rec.durable.Node.State == subagent.NodeStopped || m.sessionStoppingLocked(rec.sessionID) {
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
		return fmt.Errorf("subagent %q is unreachable: restore persistent session: %w", id, err)
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
	if m.closed || current == nil || current.durable.Node.State == subagent.NodeStopped || m.sessionStoppingLocked(sessionID) {
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
	requestID, err := newSessionRequestID()
	if err != nil {
		return "", err
	}
	_, err = m.sendCommunicationToChild(m.ctx, parentID, id, body, requestID, subagent.DeliveryGuidance)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec := m.children[id]; rec != nil {
		return rec.name, nil
	}
	return "", nil
}

func (m *subagentManager) sendCommunicationToChild(ctx context.Context, parentID string, id subagent.NodeID, body, requestID string, mode subagent.DeliveryMode) (subagent.DeliveryReceipt, error) {
	receipt := subagent.DeliveryReceipt{RequestID: requestID, Target: string(id)}
	transition := m.transition(m.rootSessionLockedSafe(parentID))
	transition.Lock()
	defer transition.Unlock()
	parentDriver, known := m.r.sessionDrivers.Lookup(parentID)
	if (known && !m.r.sessionDelegationEnabled(parentDriver.session())) || (!known && !m.r.UseSubagents()) {
		return receipt, errSubagentsDisabled
	}
	m.mu.Lock()
	rec := m.children[id]
	if rec == nil {
		m.mu.Unlock()
		return receipt, &SessionError{Kind: SessionErrorNotFound, RequestID: requestID, Operation: SessionOperationSend, Detail: fmt.Sprintf("no subagent with id %q", id)}
	}
	if rec.parentSession != parentID {
		m.mu.Unlock()
		receipt.Rejection = "unauthorized"
		return receipt, &SessionError{Kind: SessionErrorInvalid, RequestID: requestID, Operation: SessionOperationSend, Detail: fmt.Sprintf("subagent %q is not one of yours", id)}
	}
	if rec.durable.Node.State == subagent.NodeStopped || m.sessionStoppingLocked(rec.sessionID) {
		m.mu.Unlock()
		return receipt, &SessionError{Kind: SessionErrorStopped, RequestID: requestID, Operation: SessionOperationSend, Detail: fmt.Sprintf("subagent %q has been stopped; spawn a new one", id)}
	}
	sessionID, senderName := rec.sessionID, rec.parentAgentName
	m.mu.Unlock()
	if err := m.ensureChildDriver(ctx, id); err != nil {
		return receipt, err
	}
	if parent, ok := m.r.sessionDrivers.Lookup(parentID); ok {
		senderName = parent.AgentName()
	}
	// Rejected sends are retriable with the same identity; never blindly replay
	// after ambiguous storage errors or successful inbox admission.
	return m.postAgentCommunication(ctx, sessionID, agentCommunication(body, requestID, parentID, senderName, mode))
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
		if rec.durable.Node.State == subagent.NodeStopped {
			continue
		}
		if rec.parentSession != sessionID {
			continue
		}
		if m.r.sessionDrivers != nil {
			if d, ok := m.r.sessionDrivers.Lookup(rec.sessionID); ok {
				d.mu.Lock()
				active := d.running() || d.starting() || d.settling() || len(d.pending) != 0
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
func (m *subagentManager) readChild(parentID string, id subagent.NodeID) (childRead, error) {
	return m.readOwnedChild(parentID, id, true)
}

func (m *subagentManager) readRootChild(parentID string, id subagent.NodeID) (childRead, error) {
	return m.readOwnedChild(parentID, id, false)
}

func (m *subagentManager) readOwnedChild(parentID string, id subagent.NodeID, directChild bool) (childRead, error) {
	m.mu.Lock()
	rec := m.children[id]
	if rec == nil {
		m.mu.Unlock()
		return childRead{}, fmt.Errorf("no subagent with id %q", id)
	}
	if (directChild && rec.parentSession != parentID) || m.rootSessionLocked(rec.sessionID) != m.rootSessionLocked(parentID) {
		m.mu.Unlock()
		return childRead{}, fmt.Errorf("subagent %q is not one of yours", id)
	}
	m.mu.Unlock()
	snapshot, ok := m.Read(id)
	if !ok {
		return childRead{}, fmt.Errorf("no subagent with id %q", id)
	}
	return snapshot, nil
}

func (m *subagentManager) Read(id subagent.NodeID) (childRead, bool) {
	m.mu.Lock()
	rec, ok := m.children[id]
	if !ok {
		m.mu.Unlock()
		return childRead{}, false
	}
	snapshot := childRead{childRecord: *rec, state: rec.durable.Node.State, result: rec.durable.Result, errMsg: rec.durable.Node.Error}
	m.mu.Unlock()
	if d, ok := m.r.sessionDrivers.Lookup(snapshot.sessionID); ok {
		snapshot.session = d.session()
		if snapshot.state != subagent.NodeStopped {
			d.mu.Lock()
			if d.starting() {
				snapshot.state = subagent.NodeStarting
			} else if d.running() || d.settling() {
				snapshot.state = subagent.NodeRunning
			}
			d.mu.Unlock()
		}
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
	for _, rec := range stopped {
		if m.r.sessionStore != nil {
			if err := m.r.sessionStore.DeleteSession(ctx, rec.sessionID); err != nil && !errors.Is(err, session.ErrNotFound) {
				return err
			}
		}
	}
	// Remove authority handles only after every descendant write/delete has
	// succeeded. Failed deletes retain their identities for the same retry.
	m.metricsMu.Lock()
	m.mu.Lock()
	for id := range deleting {
		if tracked := m.sessions[id]; tracked != nil && tracked.unwatch != nil {
			tracked.unwatch()
		}
		delete(m.sessions, id)
	}
	var targetNode subagent.NodeID
	for id, rec := range m.children {
		if rec.sessionID == sessionID {
			targetNode = id
		}
		if deleting[rec.sessionID] {
			delete(m.children, id)
		}
	}
	// Descendants precede ancestors in removal; tree is only a projection.
	for _, rec := range slices.Backward(stopped) {
		_ = m.tree.Remove(rec.durable.Node.ID)
	}
	if targetNode != "" {
		_ = m.tree.Remove(targetNode)
	}
	for _, node := range m.tree.Snapshot().Nodes {
		if node.Node.ID == subagent.SessionRootID(sessionID) {
			_ = m.tree.Remove(node.Node.ID)
		}
	}
	m.mu.Unlock()
	m.metricsMu.Unlock()
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
	select {
	case <-m.wg.DoneChan():
	case <-ctx.Done():
		return ctx.Err()
	}
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
		return persist.closeContext(ctx)
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
	return r.subagentAttachInfo(id, true)
}

// SubagentViewInfo resolves presentation identity without restoring execution.
func (r *LocalRuntime) SubagentViewInfo(id subagent.NodeID) (SubagentAttachInfo, bool) {
	return r.subagentAttachInfo(id, false)
}

func (r *LocalRuntime) subagentAttachInfo(id subagent.NodeID, initialize bool) (SubagentAttachInfo, bool) {
	rec, ok := r.subagents.Read(id)
	if !ok {
		return SubagentAttachInfo{}, false
	}
	if initialize && rec.state != subagent.NodeStopped {
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
