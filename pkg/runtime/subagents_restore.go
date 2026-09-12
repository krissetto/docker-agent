package runtime

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// Restoring a persisted subagent swarm: normalize the stored snapshot,
// validate the complete graph and resolve every stored session before any
// driver, tree node or manager record is published, then commit the batch
// atomically.

func normalizeRestoredSnapshot(snapshot subagent.Snapshot, durability subagent.Durability) (subagent.Snapshot, error) {
	if snapshot.Version == 0 {
		snapshot.Version = subagent.SnapshotVersion
	}
	if snapshot.Version != subagent.SnapshotVersion {
		return subagent.Snapshot{}, fmt.Errorf("unsupported topology version %d", snapshot.Version)
	}
	if snapshot.Durability == "" {
		snapshot.Durability = durability
	}
	var normalize func([]subagent.NodeSnapshot, bool)
	normalize = func(nodes []subagent.NodeSnapshot, ancestorStopped bool) {
		for i := range nodes {
			state := nodes[i].Node.State
			nodeStopped := ancestorStopped || state == subagent.NodeStopped
			if nodeStopped {
				nodes[i].Node.State = subagent.NodeStopped
			} else if state == subagent.NodeStarting || state == subagent.NodeRunning {
				nodes[i].Node.State = subagent.NodeIdle
			}
			normalize(nodes[i].Children, nodeStopped)
		}
	}
	normalize(snapshot.Nodes, false)
	return snapshot, nil
}

// Restore adopts a persisted swarm snapshot for a reloaded session. Resumable
// children are rebuilt as idle sessions; explicitly stopped children keep their
// stopped state but retain transcripts for read/attach. Children whose
// sub-session or agent can no longer be loaded are shown as stopped. No-op when
// the session is already tracked in this process (an in-process switch back).
// Returns the live snapshot after adoption, or a normalization error without
// mutating runtime state.
func (m *subagentManager) Restore(ctx context.Context, sess *session.Session, snapshot subagent.Snapshot) (subagent.Snapshot, error) {
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	normalized, err := normalizeRestoredSnapshot(snapshot, m.storeDurability())
	if err != nil {
		return subagent.Snapshot{}, err
	}
	return m.restoreLocked(ctx, sess, normalized)
}

// storeDurability reports the durability of the runtime's subagent store.
func (m *subagentManager) storeDurability() subagent.Durability {
	if store, ok := m.r.subagentStore.(subagent.DurableStore); ok {
		return store.Durability()
	}
	return subagent.DurabilityVolatile
}

type restoredNode struct {
	snapshot        subagent.NodeSnapshot
	parentSessionID string
	parentSess      *session.Session
	childSess       *session.Session
	childAgent      *agent.Agent
	state           subagent.NodeState
	reservation     *restoreDriverReservation
	unwatch         func()
}

// preflightRestore validates the complete persisted graph and resolves stored
// sessions without publishing drivers, tree nodes, or manager records.
func (m *subagentManager) preflightRestore(ctx context.Context, rootSess *session.Session, snapshot subagent.Snapshot) ([]*restoredNode, error) {
	expectedRoot := subagent.SessionRootID(rootSess.ID)
	if snapshot.Root != expectedRoot || len(snapshot.Nodes) != 1 || snapshot.Nodes[0].Node.ID != expectedRoot {
		return nil, fmt.Errorf("invalid restored topology: expected exactly root %q", expectedRoot)
	}
	root := snapshot.Nodes[0].Node
	if root.Parent != "" || root.SessionID != "" || root.Agent == "" {
		return nil, errors.New("invalid restored topology: malformed root")
	}
	expectedAgent := rootSess.AgentName
	if expectedAgent == "" {
		expectedAgent = rootSess.AttributesSnapshot()[SessionAgentAttribute]
	}
	if expectedAgent == "" {
		defaultAgent, err := m.r.team.DefaultAgent()
		if err != nil {
			return nil, fmt.Errorf("invalid restored topology: resolve root agent: %w", err)
		}
		expectedAgent = defaultAgent.Name()
	}
	if root.Agent != expectedAgent {
		return nil, fmt.Errorf("invalid restored topology: root agent binding %q does not match %q", root.Agent, expectedAgent)
	}
	rootAgent, err := m.r.team.Agent(root.Agent)
	if err != nil {
		return nil, fmt.Errorf("invalid restored topology: root agent %q: %w", root.Agent, err)
	}

	// Validate the persisted graph independently of the current team config.
	// Config drift may stop nodes below, but must never hide corruption.
	seenIDs := map[subagent.NodeID]struct{}{expectedRoot: {}}
	seenSessions := map[string]struct{}{}
	var validateStructure func([]subagent.NodeSnapshot, subagent.Node) error
	validateStructure = func(nodes []subagent.NodeSnapshot, parent subagent.Node) error {
		for _, snap := range nodes {
			node := snap.Node
			if node.ID == "" || node.Agent == "" {
				return errors.New("invalid restored topology: child id and agent are required")
			}
			terminalSessionless := node.SessionID == "" && (node.State == subagent.NodeFailed || node.State == subagent.NodeStopped)
			if node.SessionID == "" && !terminalSessionless {
				return errors.New("invalid restored topology: active child session id is required")
			}
			if terminalSessionless && len(snap.Children) != 0 {
				return errors.New("invalid restored topology: sessionless terminal child cannot have descendants")
			}
			if node.Parent != parent.ID {
				return fmt.Errorf("invalid restored topology: node %q parent %q does not match enclosing %q", node.ID, node.Parent, parent.ID)
			}
			if _, duplicate := seenIDs[node.ID]; duplicate {
				return fmt.Errorf("invalid restored topology: duplicate or cyclic node id %q", node.ID)
			}
			seenIDs[node.ID] = struct{}{}
			if node.SessionID != "" {
				if _, duplicate := seenSessions[node.SessionID]; duplicate {
					return fmt.Errorf("invalid restored topology: duplicate child session id %q", node.SessionID)
				}
				seenSessions[node.SessionID] = struct{}{}
			}
			if err := validateStructure(snap.Children, node); err != nil {
				return err
			}
		}
		return nil
	}
	if err := validateStructure(snapshot.Nodes[0].Children, root); err != nil {
		return nil, err
	}

	var prepared []*restoredNode
	var walk func([]subagent.NodeSnapshot, string, *session.Session, *agent.Agent, bool) error
	walk = func(nodes []subagent.NodeSnapshot, parentSessionID string, parentSess *session.Session, parentAgent *agent.Agent, ancestorStopped bool) error {
		var allowed []subagent.AllowedSubagent
		if !ancestorStopped {
			allowed = m.r.allowedFromAgent(parentAgent)
		}
		for _, snap := range nodes {
			node := snap.Node
			var childSess *session.Session
			if node.SessionID != "" && m.r.sessionStore != nil {
				loaded, loadErr := m.r.sessionStore.GetSession(ctx, node.SessionID)
				switch {
				case loadErr == nil && loaded != nil:
					if loaded.ParentID != parentSessionID {
						return fmt.Errorf("invalid restored topology: session %q parent %q does not match enclosing session", node.SessionID, loaded.ParentID)
					}
					if binding := loaded.AttributesSnapshot()[SessionAgentAttribute]; binding != "" && binding != node.Agent {
						return fmt.Errorf("invalid restored topology: session %q session binding %q does not match node agent %q", node.SessionID, binding, node.Agent)
					}
					loaded = loaded.Clone()
					loaded.AgentName = node.Agent
					loaded.NonInteractive = true
					loaded.AsyncSubagent = true
					childSess = loaded
				case loadErr != nil && !errors.Is(loadErr, session.ErrNotFound):
					return fmt.Errorf("load restored child session %q: %w", node.SessionID, loadErr)
				}
			}

			state := node.State
			var childAgent *agent.Agent
			if ancestorStopped || state == subagent.NodeStopped {
				state = subagent.NodeStopped
			} else {
				candidate, resolveErr := m.r.team.Agent(node.Agent)
				if resolveErr == nil {
					childAgent = candidate
				}
				allowedAgent := false
				for _, ref := range allowed {
					if ref.Agent == node.Agent {
						allowedAgent = true
						break
					}
				}
				if resolveErr != nil || !allowedAgent || childSess == nil {
					state = subagent.NodeStopped
					childAgent = nil
				} else if state != subagent.NodeFailed {
					state = subagent.NodeIdle
				}
			}
			entry := &restoredNode{snapshot: snap, parentSessionID: parentSessionID, parentSess: parentSess, childSess: childSess, childAgent: childAgent, state: state}
			prepared = append(prepared, entry)
			if err := walk(snap.Children, node.SessionID, childSess, childAgent, state == subagent.NodeStopped); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(snapshot.Nodes[0].Children, rootSess.ID, rootSess, rootAgent, false); err != nil {
		return nil, err
	}
	return prepared, nil
}

// restoreLocked performs all fallible validation and driver initialization
// before atomically publishing the topology under m.mu.
func (m *subagentManager) restoreLocked(ctx context.Context, sess *session.Session, snapshot subagent.Snapshot) (subagent.Snapshot, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return subagent.Snapshot{}, nil
	}
	_, tracked := m.sessions[sess.ID]
	m.mu.Unlock()
	if tracked {
		snapshot, _ := snapshotForRoot(m.tree.Snapshot(), subagent.SessionRootID(sess.ID))
		return snapshot, nil
	}

	prepared, err := m.preflightRestore(ctx, sess, snapshot)
	if err != nil {
		return subagent.Snapshot{}, err
	}
	root := snapshot.Nodes[0].Node
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return subagent.Snapshot{}, nil
	}
	if _, exists := m.sessions[sess.ID]; exists {
		m.mu.Unlock()
		return subagent.Snapshot{}, fmt.Errorf("invalid restored topology: root session %q is already registered", sess.ID)
	}
	if _, exists := m.tree.Node(root.ID); exists {
		m.mu.Unlock()
		return subagent.Snapshot{}, fmt.Errorf("invalid restored topology: root node %q already exists", root.ID)
	}
	for _, entry := range prepared {
		node := entry.snapshot.Node
		if _, exists := m.tree.Node(node.ID); exists {
			m.mu.Unlock()
			return subagent.Snapshot{}, fmt.Errorf("invalid restored topology: node id %q collides with live topology", node.ID)
		}
		if _, exists := m.children[node.ID]; exists {
			m.mu.Unlock()
			return subagent.Snapshot{}, fmt.Errorf("invalid restored topology: node id %q collides with live child", node.ID)
		}
		if node.SessionID != "" {
			if _, exists := m.sessions[node.SessionID]; exists {
				m.mu.Unlock()
				return subagent.Snapshot{}, fmt.Errorf("invalid restored topology: child session %q collides with live topology", node.SessionID)
			}
			if _, exists := m.r.sessionDrivers.Lookup(node.SessionID); exists {
				m.mu.Unlock()
				return subagent.Snapshot{}, fmt.Errorf("invalid restored topology: child session %q already has a driver", node.SessionID)
			}
		}
	}

	initialized := make([]*restoredNode, 0, len(prepared))
	releaseInitialized := func() {
		for _, entry := range initialized {
			if entry.unwatch != nil {
				entry.unwatch()
			}
			entry.reservation.Discard()
		}
	}
	for _, entry := range prepared {
		if entry.state == subagent.NodeStopped || entry.childSess == nil {
			continue
		}
		reservation, initErr := m.r.sessionDrivers.PrepareRestore(ctx, entry.childSess)
		if initErr != nil {
			m.mu.Unlock()
			releaseInitialized()
			return subagent.Snapshot{}, initErr
		}
		entry.reservation = reservation
		driver := reservation.driver
		childID := entry.snapshot.Node.ID
		driver.SetPreStartErrorGate(func() error { return m.admitChildRun(childID) }, func() { m.abortChildStart(childID) })
		unwatchSettled := driver.OnSettled(func() { m.reportChildSettled(childID) })
		unwatchStarted := driver.OnStarted(func() { m.markChildRunning(childID) })
		entry.unwatch = func() { unwatchSettled(); unwatchStarted() }
		initialized = append(initialized, entry)
	}

	// Add this root and its descendants as one atomic tree update. Existing
	// roots remain untouched, including their timestamps.
	treeNodes := make([]subagent.Node, 0, len(prepared)+1)
	treeNodes = append(treeNodes, root)
	for _, entry := range prepared {
		node := entry.snapshot.Node
		node.State = entry.state
		treeNodes = append(treeNodes, node)
	}

	insertedSessions := []string{sess.ID}
	insertedChildren := make([]subagent.NodeID, 0, len(prepared))
	m.sessions[sess.ID] = &sessionSubagents{node: root.ID, topLevel: true, sess: sess}
	for _, entry := range prepared {
		node := entry.snapshot.Node
		rec := &childRecord{name: node.DisplayName(), parentSession: entry.parentSessionID, sessionID: node.SessionID, session: entry.childSess, parentSess: entry.parentSess, agent: entry.childAgent, state: entry.state, errMsg: node.Error, unwatch: entry.unwatch}
		if entry.childSess != nil {
			rec.result = entry.childSess.GetLastAssistantMessageContent()
		}
		m.children[node.ID] = rec
		insertedChildren = append(insertedChildren, node.ID)
		if entry.reservation != nil {
			m.sessions[node.SessionID] = &sessionSubagents{node: node.ID}
			insertedSessions = append(insertedSessions, node.SessionID)
		}
	}
	reservations := make([]*restoreDriverReservation, 0, len(initialized))
	for _, entry := range initialized {
		reservations = append(reservations, entry.reservation)
	}
	if err := m.r.sessionDrivers.ActivateRestoreBatch(reservations, func() error {
		if err := m.tree.AddSubtree(treeNodes); err != nil {
			return fmt.Errorf("publish restored topology: %w", err)
		}
		return nil
	}); err != nil {
		for _, id := range insertedChildren {
			delete(m.children, id)
		}
		for _, id := range insertedSessions {
			delete(m.sessions, id)
		}
		m.mu.Unlock()
		releaseInitialized()
		return subagent.Snapshot{}, err
	}
	m.mu.Unlock()

	// Pending accepted input becomes executable only after the complete topology,
	// ownership records, admission gates, and callbacks are externally visible.
	for _, entry := range initialized {
		entry.reservation.driver.WakePending()
	}
	m.persistSnapshot()
	live, ok := snapshotForRoot(m.tree.Snapshot(), root.ID)
	if !ok {
		return subagent.Snapshot{}, errors.New("restored topology root disappeared")
	}
	return live, nil
}

// RestoreSubagentTree rebuilds the subagent swarm of a reloaded session from
// the subagent store. Resumable subagents stay conversational; explicitly
// stopped subagents stay stopped while remaining readable/attachable. Safe to
// call for sessions already tracked in-process.
func (r *LocalRuntime) RestoreSubagentTree(ctx context.Context, sess *session.Session) (*subagent.Snapshot, error) {
	m := r.subagents
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed || r.subagentStore == nil {
		return nil, nil
	}
	stored, err := r.subagentStore.LoadTree(ctx, sess.ID)
	if err != nil || stored == nil {
		return nil, err
	}
	snapshot, err := m.Restore(ctx, sess, *stored)
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}
