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

func normalizeRestoredSnapshot(snapshot subagent.Snapshot, durability subagent.Durability, records ...session.ChildRecord) (subagent.Snapshot, error) {
	if snapshot.Version == 0 {
		snapshot.Version = subagent.SnapshotVersion
	}
	if snapshot.Version != subagent.SnapshotVersion {
		return subagent.Snapshot{}, fmt.Errorf("unsupported topology version %d", snapshot.Version)
	}
	if snapshot.Durability == "" {
		snapshot.Durability = durability
	}
	canonical := make(map[subagent.NodeID]subagent.NodeState, len(records))
	for _, record := range records {
		canonical[record.Node.ID] = record.Node.State
	}
	var normalize func([]subagent.NodeSnapshot, bool)
	normalize = func(nodes []subagent.NodeSnapshot, ancestorStopped bool) {
		for i := range nodes {
			state := nodes[i].Node.State
			inheritedStop := ancestorStopped
			// Canonical stop is subtree-atomic; a later manual revival is node-local.
			if stored, ok := canonical[nodes[i].Node.ID]; ok {
				state = stored
				nodes[i].Node.State = stored
				inheritedStop = false
			}
			nodeStopped := inheritedStop || state == subagent.NodeStopped
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

// Restore is retained for compatibility; persisted coordination is authoritative.
func (m *subagentManager) Restore(ctx context.Context, sess *session.Session, _ subagent.Snapshot) (subagent.Snapshot, error) {
	restored, err := m.r.RestoreSubagentTree(ctx, sess)
	if err != nil {
		return subagent.Snapshot{}, err
	}
	if restored == nil {
		return subagent.Snapshot{}, nil
	}
	return *restored, nil
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
			activeName := node.Agent
			if childSess != nil && childSess.AgentName != "" {
				activeName = childSess.AgentName
			}
			candidate, resolveErr := m.r.team.Agent(activeName)
			allowedAgent := false
			for _, ref := range allowed {
				if ref.Agent == node.Agent {
					allowedAgent = true
					break
				}
			}
			invalid := ancestorStopped || resolveErr != nil || !allowedAgent || childSess == nil
			if invalid {
				state = subagent.NodeStopped
			} else {
				childAgent = candidate
				if state != subagent.NodeFailed && state != subagent.NodeStopped {
					state = subagent.NodeIdle
				}
			}
			entry := &restoredNode{snapshot: snap, parentSessionID: parentSessionID, parentSess: parentSess, childSess: childSess, childAgent: childAgent, state: state}
			prepared = append(prepared, entry)
			if err := walk(snap.Children, node.SessionID, childSess, childAgent, invalid); err != nil {
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

// RestoreSubagentTree rebuilds the subagent swarm of a reloaded session from
// persisted coordination through confirmed dormant acquisition. Existing live
// owners are reused; no interrupted execution is replayed.
func (r *LocalRuntime) RestoreSubagentTree(ctx context.Context, sess *session.Session) (*subagent.Snapshot, error) {
	if sess == nil {
		return nil, &SessionError{Kind: SessionErrorInvalid, Operation: "restore_tree"}
	}
	committed, err := RestoreSessionView(ctx, &localSessionRuntimeView{runtime: r}, sess.ID)
	if err != nil {
		return nil, err
	}
	return committed.Info.Session.GetSubagentTree(), nil
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
	// Child records own identity, topology and lifecycle. Usage counters are
	// maintained by the tree projection, not child transition commits.
	projected := map[subagent.NodeID]subagent.Node{}
	var collectMetrics func([]subagent.NodeSnapshot)
	collectMetrics = func(nodes []subagent.NodeSnapshot) {
		for _, item := range nodes {
			projected[item.Node.ID] = item.Node
			collectMetrics(item.Children)
		}
	}
	collectMetrics(legacy.Nodes)
	mergeMetrics := func(node subagent.Node) subagent.Node {
		if projection, ok := projected[node.ID]; ok && projection.SessionID == node.SessionID {
			node.Cost = max(node.Cost, projection.Cost)
			node.InputTokens = max(node.InputTokens, projection.InputTokens)
			node.OutputTokens = max(node.OutputTokens, projection.OutputTokens)
			node.ToolCalls = max(node.ToolCalls, projection.ToolCalls)
		}
		return node
	}
	rootNode = mergeMetrics(rootNode)
	children := map[subagent.NodeID][]subagent.Node{}
	known := map[subagent.NodeID]bool{}
	for _, record := range records {
		children[record.Node.Parent] = append(children[record.Node.Parent], mergeMetrics(record.Node))
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
	visited := map[subagent.NodeID]bool{}
	snap, err := build(rootNode, visited)
	if err == nil {
		for _, record := range records {
			if !visited[record.Node.ID] {
				return subagent.Snapshot{}, nil, fmt.Errorf("invalid restored topology: unreachable canonical child %q", record.Node.ID)
			}
		}
	}
	return subagent.Snapshot{Version: subagent.SnapshotVersion, Durability: m.storeDurability(), Root: rootNode.ID, Nodes: []subagent.NodeSnapshot{snap}}, records, err
}
