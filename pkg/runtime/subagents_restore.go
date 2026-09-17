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
	canonical, records, err := m.canonicalSnapshot(ctx, sess, snapshot)
	if err != nil {
		return subagent.Snapshot{}, err
	}
	normalized, err := normalizeRestoredSnapshot(canonical, m.storeDurability())
	if err != nil {
		return subagent.Snapshot{}, err
	}
	restored, err := m.restoreLockedRecords(ctx, sess, normalized, records)
	if err == nil {
		m.mu.Lock()
		for _, record := range records {
			if child := m.children[record.Node.ID]; child != nil {
				child.durable = record
				child.result = record.Result
			}
		}
		m.mu.Unlock()
		m.r.sessionDrivers.signalWork()
	}
	return restored, err
}

// storeDurability reports the durability of the runtime's subagent store.
func (m *subagentManager) storeDurability() subagent.Durability {
	return m.r.sessionDurability()
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
	expectedAgent := rootSess.AttributesSnapshot()[SessionAgentAttribute]
	if expectedAgent == "" {
		expectedAgent = rootSess.AgentName
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
					if loaded.AgentName == "" {
						loaded.AgentName = node.Agent
					}
					loaded.NonInteractive = true
					loaded.AsyncSubagent = true
					childSess = loaded
				case loadErr != nil && !errors.Is(loadErr, session.ErrNotFound):
					return fmt.Errorf("load restored child session %q: %w", node.SessionID, loadErr)
				}
			}

			grant := parentAgent
			if childSess != nil {
				if provenance := childSess.AttributesSnapshot()[SessionParentAgentAttribute]; provenance != "" {
					grant, _ = m.r.team.Agent(provenance)
				}
			}
			allowed = nil
			if grant != nil && !ancestorStopped {
				allowed = m.r.allowedFromAgent(grant)
			}
			state := node.State
			var childAgent *agent.Agent
			if ancestorStopped || state == subagent.NodeStopped {
				state = subagent.NodeStopped
			} else {
				activeName := node.Agent
				if childSess != nil && childSess.AgentName != "" {
					activeName = childSess.AgentName
				}
				candidate, resolveErr := m.r.team.Agent(activeName)
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
func (m *subagentManager) restoreLockedRecords(ctx context.Context, sess *session.Session, snapshot subagent.Snapshot, records []session.ChildRecord) (subagent.Snapshot, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return subagent.Snapshot{}, nil
	}
	_, tracked := m.sessions[sess.ID]
	m.mu.Unlock()
	if tracked {
		m.mu.Lock()
		hasChildren := false
		for _, rec := range m.children {
			if m.rootSessionLocked(rec.parentSession) == sess.ID {
				hasChildren = true
				break
			}
		}
		m.mu.Unlock()
		if hasChildren {
			snapshot, _ := snapshotForRoot(m.tree.Snapshot(), subagent.SessionRootID(sess.ID))
			return snapshot, nil
		}
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
	if tracked := m.sessions[sess.ID]; tracked != nil {
		// A root may receive a report while the durable descendants are preflighted.
		// Replace only its synthetic projection, never a published child graph.
		if tracked.unwatch != nil {
			tracked.unwatch()
		}
		delete(m.sessions, sess.ID)
		_ = m.tree.Remove(root.ID)
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
		entry.unwatch = driver.OnStarted(func() { m.markChildRunning(childID) })
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
	m.sessions[sess.ID] = &sessionSubagents{node: root.ID, topLevel: true}
	for _, entry := range prepared {
		node := entry.snapshot.Node
		rec := &childRecord{name: node.DisplayName(), parentSession: entry.parentSessionID, sessionID: node.SessionID, agent: entry.childAgent, state: entry.state, errMsg: node.Error, unwatch: entry.unwatch}
		if entry.childSess != nil {
			rec.parentAgentName = entry.childSess.AttributesSnapshot()[SessionParentAgentAttribute]
			if rec.parentAgentName == "" && entry.parentSess != nil {
				rec.parentAgentName = entry.parentSess.AgentName
			}
			rec.result = ownAssistantResult(entry.childSess)
		}
		for _, record := range records {
			if record.Node.ID == node.ID {
				rec.durable = record
				rec.result = record.Result
				break
			}
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
		for _, entry := range prepared {
			rec := m.children[entry.snapshot.Node.ID]
			if rec.durable.Revision != 0 || entry.childSess == nil {
				continue
			}
			node := entry.snapshot.Node
			node.State = entry.state
			preview, _ := subagent.PreviewText(rec.result, subagent.PreviewLen)
			record := session.ChildRecord{RootSessionID: sess.ID, ParentSessionID: entry.parentSessionID, Node: node, Revision: 1, Result: preview, Error: node.Error}
			admission := entry.childSess.OwnSnapshot()
			admission.Messages = nil
			if err := m.coordination().AdmitChild(ctx, session.ChildAdmission{Child: admission, Record: record}); err != nil {
				return fmt.Errorf("adopt legacy child %s: %w", node.ID, err)
			}
			rec.durable = record
		}
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
	if err != nil {
		return nil, err
	}
	if stored == nil {
		records, loadErr := m.coordination().LoadChildren(ctx, sess.ID)
		if loadErr != nil {
			return nil, loadErr
		}
		if len(records) == 0 {
			return nil, nil
		}
		stored = &subagent.Snapshot{}
	}
	snapshot, err := m.Restore(ctx, sess, *stored)
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (m *subagentManager) canonicalSnapshot(ctx context.Context, root *session.Session, legacy subagent.Snapshot) (subagent.Snapshot, []session.ChildRecord, error) {
	return m.canonicalSnapshotWithLimit(ctx, root, legacy, -1)
}

// canonicalSnapshotWithLimit bounds detached view hydration, including stopped
// history, independently of driver residency. A negative limit retains general
// Restore's existing behavior; view preparation always supplies a finite limit.
func (m *subagentManager) canonicalSnapshotWithLimit(ctx context.Context, root *session.Session, legacy subagent.Snapshot, limit int) (subagent.Snapshot, []session.ChildRecord, error) {
	capacityError := func() error {
		return &SessionError{Kind: SessionErrorCapacity, SessionID: root.ID, Operation: "restore_tree", Reason: SessionErrorReasonLimit, Limit: limit}
	}
	if limit >= 0 {
		if err := ctx.Err(); err != nil {
			return subagent.Snapshot{}, nil, err
		}
		// Count before any recursive normalization, merge or construction. Check
		// widths before copying them so malformed broad trees cannot grow work.
		if limit == 0 || len(legacy.Nodes) > limit {
			return subagent.Snapshot{}, nil, capacityError()
		}
		count := len(legacy.Nodes)
		pending := append([]subagent.NodeSnapshot(nil), legacy.Nodes...)
		for len(pending) != 0 {
			if err := ctx.Err(); err != nil {
				return subagent.Snapshot{}, nil, err
			}
			node := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if len(node.Children) > limit-count {
				return subagent.Snapshot{}, nil, capacityError()
			}
			count += len(node.Children)
			pending = append(pending, node.Children...)
		}
	}
	// The storage API returns all records at once; bound processing immediately
	// after that read, before allocating maps or recursively building the tree.
	records, err := m.coordination().LoadChildren(ctx, root.ID)
	if err != nil {
		return subagent.Snapshot{}, nil, err
	}
	if limit >= 0 {
		if err := ctx.Err(); err != nil {
			return subagent.Snapshot{}, nil, err
		}
		if len(records) > limit-1 {
			return subagent.Snapshot{}, nil, capacityError()
		}
	}
	if len(records) == 0 {
		return legacy, nil, nil
	}
	agentName := root.AttributesSnapshot()[SessionAgentAttribute]
	if agentName == "" {
		agentName = root.AgentName
	}
	if agentName == "" && m.r.team != nil {
		if defaultAgent, err := m.r.team.DefaultAgent(); err == nil {
			agentName = defaultAgent.Name()
		}
	}
	rootNode := subagent.Node{ID: subagent.SessionRootID(root.ID), Agent: agentName, State: subagent.NodeIdle}
	children := map[subagent.NodeID][]subagent.Node{}
	known := map[subagent.NodeID]bool{}
	for _, record := range records {
		children[record.Node.Parent] = append(children[record.Node.Parent], record.Node)
		known[record.Node.ID] = true
	}
	// A failed multi-row legacy migration is retryable: canonical rows win, but
	// not-yet-adopted nodes remain in preflight and cannot silently disappear.
	mergedCount := len(records) + 1 // include the canonical root and every record
	var mergeLegacy func([]subagent.NodeSnapshot) error
	mergeLegacy = func(nodes []subagent.NodeSnapshot) error {
		for _, item := range nodes {
			if limit >= 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			if item.Node.ID != rootNode.ID && !known[item.Node.ID] {
				if limit >= 0 && mergedCount >= limit {
					return capacityError()
				}
				mergedCount++
				children[item.Node.Parent] = append(children[item.Node.Parent], item.Node)
				known[item.Node.ID] = true
			}
			if err := mergeLegacy(item.Children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := mergeLegacy(legacy.Nodes); err != nil {
		return subagent.Snapshot{}, nil, err
	}
	var build func(subagent.Node, map[subagent.NodeID]bool) (subagent.NodeSnapshot, error)
	build = func(node subagent.Node, seen map[subagent.NodeID]bool) (subagent.NodeSnapshot, error) {
		if limit >= 0 {
			if err := ctx.Err(); err != nil {
				return subagent.NodeSnapshot{}, err
			}
		}
		if seen[node.ID] {
			return subagent.NodeSnapshot{}, errors.New("cyclic child records")
		}
		seen[node.ID] = true
		snap := subagent.NodeSnapshot{Node: node}
		for _, child := range children[node.ID] {
			branch, err := build(child, seen)
			if err != nil {
				return snap, err
			}
			snap.Children = append(snap.Children, branch)
		}
		return snap, nil
	}
	snap, err := build(rootNode, map[subagent.NodeID]bool{})
	return subagent.Snapshot{Version: subagent.SnapshotVersion, Durability: m.storeDurability(), Root: rootNode.ID, Nodes: []subagent.NodeSnapshot{snap}}, records, err
}
