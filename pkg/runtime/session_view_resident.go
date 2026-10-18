package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// Resident views recheck persisted identity along the selected ancestry. Cold
// trees and stores without targeted reads still use the complete restore preflight.
func (v *localSessionRuntimeView) readResidentSessionView(ctx context.Context, id string) (*preparedSessionView, error) {
	r := v.runtime
	metadata, metadataOK := r.sessionStore.(session.MetadataStore)
	children, childrenOK := r.sessionStore.(session.ChildRecordReader)
	if !metadataOK || !childrenOK {
		return nil, nil
	}
	if _, ok := r.sessionDrivers.Lookup(id); !ok {
		return nil, nil
	}
	invalid := func(operation string) error {
		return &SessionError{Kind: SessionErrorConflict, SessionID: id, Operation: SessionOperation(operation)}
	}
	limit := r.maxSessions
	if limit < 0 {
		limit = DefaultSessionResourcePolicy().MaxSessions
	}
	p := &preparedSessionView{r: r, resident: true, existing: make(map[string]*sessionDriver)}
	var ancestry []*session.Session
	for current := id; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(ancestry) >= limit {
			return nil, &SessionError{Kind: SessionErrorCapacity, SessionID: id, Operation: "restore_ancestry", Limit: limit, Reason: SessionErrorReasonLimit}
		}
		if _, seen := p.existing[current]; seen {
			return nil, invalid("restore_ancestry")
		}
		row, err := metadata.GetSessionMetadata(ctx, current)
		if err != nil {
			return nil, err
		}
		if row == nil || row.ID != current {
			return nil, invalid("restore_ancestry")
		}
		driver, ok := r.sessionDrivers.Lookup(current)
		if !ok {
			return nil, nil
		}
		projection := driver.registrySnapshot()
		if projection.reclaiming || projection.stopped && !projection.stoppedView {
			return nil, nil
		}
		driver.mu.Lock()
		live := driver.sess.MetadataSnapshot()
		driver.mu.Unlock()
		if live == nil {
			return nil, ErrSessionClosed
		}
		bound := row.AttributesSnapshot()[SessionAgentAttribute]
		if bound == "" || bound != live.AttributesSnapshot()[SessionAgentAttribute] || row.AttributesSnapshot()[SessionAgentIDAttribute] != live.AttributesSnapshot()[SessionAgentIDAttribute] || row.ParentID != driver.identityParent || row.AttributesSnapshot()[SessionParentAgentAttribute] != live.AttributesSnapshot()[SessionParentAgentAttribute] || row.AttributesSnapshot()["docker-agent.actor.source"] != live.AttributesSnapshot()["docker-agent.actor.source"] {
			return nil, invalid("restore_binding")
		}
		_, currentCreation, identityOK := identityForBinding(r.teamForSession(row), row, SessionAgentAttribute, SessionAgentIDAttribute)
		if !identityOK {
			return nil, invalid("restore_binding")
		}
		if row.AgentName == "" {
			row.AgentName = currentCreation
		}
		row.AgentID = live.AgentID
		if row.AgentName != live.AgentName || row.AgentID != live.AgentID {
			return nil, invalid("restore_binding")
		}
		if live.AgentID != "" {
			if _, _, ok := r.teamForSession(row).ResolveIdentity(live.AgentID); !ok {
				return nil, invalid("restore_binding")
			}
		} else if _, _, ok := r.teamForSession(row).ResolveLegacyName(row.AgentName); !ok {
			return nil, invalid("restore_binding")
		}
		if row.WorkingDir != live.WorkingDir || row.WorkingDir != "" && !sameWorkspace(row.WorkingDir, r.workingDir) {
			return nil, &SessionError{Kind: SessionErrorWrongSession, SessionID: id, Operation: "view_workspace"}
		}
		p.existing[current] = driver
		ancestry = append(ancestry, row)
		if row.ParentID == "" {
			p.root = row
			break
		}
		current = row.ParentID
	}
	root := ancestry[len(ancestry)-1]
	for i := len(ancestry) - 2; i >= 0; i-- {
		row, parent := ancestry[i], ancestry[i+1]
		if source := row.AttributesSnapshot()["docker-agent.actor.source"]; source != "" && source != parent.AttributesSnapshot()["docker-agent.actor.source"] {
			return nil, invalid("restore_source")
		}
		record, err := children.LoadChild(ctx, row.ID)
		if err != nil {
			return nil, err
		}
		parentNode := subagent.SessionRootID(root.ID)
		if i+1 < len(ancestry)-1 {
			parentNode = p.records[len(p.records)-1].Node.ID
		}
		if record.RootSessionID != root.ID || record.ParentSessionID != parent.ID || record.Node.SessionID != row.ID || record.Node.Parent != parentNode || record.Node.Agent != row.AttributesSnapshot()[SessionAgentAttribute] {
			return nil, invalid("restore_tree_membership")
		}
		childIdentity, _, childIdentityOK := identityForBinding(r.teamForSession(row), row, SessionAgentAttribute, SessionAgentIDAttribute)
		if !childIdentityOK {
			return nil, invalid("restore_tree_membership")
		}
		grant, _, grantOK := identityForBinding(r.teamForSession(row), row, SessionParentAgentAttribute, SessionParentAgentIDAttribute)
		if !grantOK {
			grant, _, grantOK = resolveAgentIdentity(r.teamForSession(parent), parent.AgentID, parent.AgentName)
		}
		if !grantOK {
			return nil, invalid("restore_tree_membership")
		}
		allowed := false
		for _, ref := range r.allowedFromAgentForTeam(r.teamForSession(parent), grant) {
			if ref.Agent == childIdentity.Name() {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, invalid("restore_tree_membership")
		}
		if record.Node.State == subagent.NodeStopped && !p.existing[row.ID].registrySnapshot().stoppedView {
			return nil, nil
		}
		p.records = append(p.records, record)
	}
	r.subagents.mu.Lock()
	err := p.validateResidentTopologyLocked()
	r.subagents.mu.Unlock()
	if err != nil {
		return nil, err
	}
	selected := ancestry[0]
	driver := p.existing[id]
	snapshot := driver.session()
	if snapshot == nil {
		return nil, ErrSessionClosed
	}
	if selected.ParentID == "" {
		if tree, ok := snapshotForRoot(r.subagents.tree.Snapshot(), subagent.SessionRootID(root.ID)); ok {
			snapshot.SetSubagentTree(&tree)
		}
	}
	_, model, _ := driver.ModelBindingSnapshot()
	p.info = PreparedSessionViewInfo{SessionID: id, RootSessionID: root.ID, RootWorkingDir: root.WorkingDir, RootBinding: SessionBinding{AgentName: root.AttributesSnapshot()[SessionAgentAttribute]}, Session: snapshot,
		ActiveAgentName: snapshot.AgentName, WorkingDir: snapshot.WorkingDir, Binding: SessionBinding{AgentName: selected.AttributesSnapshot()[SessionAgentAttribute], ParentSessionID: selected.ParentID, Model: model, Durability: r.sessionDurability()}}
	if selected.ParentID != "" {
		node := p.records[len(p.records)-1].Node
		p.info.Attach = &SubagentAttachInfo{NodeID: node.ID, Name: node.DisplayName(), Agent: snapshot.AgentName, Session: snapshot.Clone(), ParentSessionID: selected.ParentID, ParentAgent: ancestry[1].AgentName}
	} else if p.residentRootNeedsColdRestore(ctx) {
		return nil, nil
	}
	return p, nil
}

// An untracked root may still have a durable child tree (for example, a
// persisted root explicitly registered into this runtime). Let the cold
// preflight validate and publish it rather than treating it as an empty root.
func (p *preparedSessionView) residentRootNeedsColdRestore(ctx context.Context) bool {
	if p.info.Session.GetSubagentTree() != nil {
		return true
	}
	m := p.r.subagents
	m.mu.Lock()
	tracked := m.sessions[p.root.ID] != nil
	_, rootExists := m.tree.Node(subagent.SessionRootID(p.root.ID))
	m.mu.Unlock()
	if tracked || rootExists {
		return false
	}
	knownEmpty := false
	if store, ok := p.r.sessionStore.(session.CoordinationStore); ok {
		records, err := store.LoadChildren(ctx, p.root.ID)
		if err != nil || len(records) != 0 {
			return true
		}
		knownEmpty = true
	}
	if p.r.subagentStore != nil {
		legacy, err := p.r.subagentStore.LoadTree(ctx, p.root.ID)
		if err != nil || legacy != nil {
			return true
		}
		knownEmpty = true
	}
	return !knownEmpty
}

// Caller holds the manager mutex; topology lookups are indexed, not snapshots.
func (p *preparedSessionView) validateResidentTopologyLocked() error {
	m := p.r.subagents
	if m.closed {
		return ErrSessionClosed
	}
	root, ok := m.tree.Node(subagent.SessionRootID(p.root.ID))
	tracked := m.sessions[p.root.ID]
	if !ok && tracked == nil && len(p.records) == 0 {
		// Fresh root creation publishes its driver before the synthetic tree root
		// is needed. An entirely unseeded root is not a membership mismatch.
		for _, child := range m.children {
			if child.parentSession == p.root.ID || child.durable.RootSessionID == p.root.ID {
				return &SessionError{Kind: SessionErrorConflict, SessionID: p.root.ID, Operation: "restore_tree_membership"}
			}
		}
		return nil
	}
	if !ok || tracked == nil || !tracked.topLevel || root.Agent != p.root.AttributesSnapshot()[SessionAgentAttribute] {
		return &SessionError{Kind: SessionErrorConflict, SessionID: p.root.ID, Operation: "restore_tree_membership"}
	}
	for _, record := range p.records {
		current := m.children[record.Node.ID]
		node, ok := m.tree.Node(record.Node.ID)
		if !ok || current == nil || !sameRestoredMembership(current.durable, record) || node.SessionID != record.Node.SessionID || node.Parent != record.Node.Parent || node.Agent != record.Node.Agent {
			return &SessionError{Kind: SessionErrorConflict, SessionID: record.Node.SessionID, Operation: "restore_tree_membership"}
		}
	}
	return nil
}

func sameRestoredMembership(a, b session.ChildRecord) bool {
	return a.RootSessionID == b.RootSessionID && a.ParentSessionID == b.ParentSessionID && a.Node.ID == b.Node.ID && a.Node.SessionID == b.Node.SessionID && a.Node.Parent == b.Node.Parent && a.Node.Agent == b.Node.Agent && a.Revision == b.Revision
}

func (p *preparedSessionView) commitResident(ctx context.Context) (CommittedSessionView, error) {
	restore := p.r.subagents.transition("restore:" + p.root.ID)
	restore.Lock()
	transition := p.r.subagents.transition(p.root.ID)
	transition.Lock()
	locked := true
	defer func() {
		if locked {
			transition.Unlock()
			restore.Unlock()
		}
	}()
	fresh, err := (&localSessionRuntimeView{runtime: p.r}).readResidentSessionView(ctx, p.info.SessionID)
	if err != nil {
		return CommittedSessionView{}, err
	}
	if fresh == nil {
		return CommittedSessionView{}, &SessionError{Kind: SessionErrorConflict, SessionID: p.info.SessionID, Operation: "commit_view"}
	}
	if err := p.info.Confirmation().Validate(fresh.info); err != nil {
		return CommittedSessionView{}, err
	}
	m, g := p.r.subagents, p.r.sessionDrivers
	m.mu.Lock()
	g.mu.Lock()
	err = p.validateRegistryLocked()
	if err == nil {
		err = fresh.validateResidentTopologyLocked()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = p.ctx().Err()
	}
	if err == nil && m.sessions[p.root.ID] == nil {
		// A fresh root has a resident owner before subagent topology is needed.
		// Seed its synthetic node only after the attachment has been confirmed.
		rootID := subagent.SessionRootID(p.root.ID)
		root := subagent.Node{ID: rootID, Agent: p.root.AttributesSnapshot()[SessionAgentAttribute], State: subagent.NodeIdle}
		if driver := p.existing[p.root.ID]; driver.Status().State == SessionStateRunning {
			root.State = subagent.NodeRunning
		}
		err = m.tree.Add(root)
		if err == nil {
			tracked := &sessionSubagents{node: rootID, topLevel: true}
			m.sessions[p.root.ID] = tracked
			driver := p.existing[p.root.ID]
			g.mu.Unlock()
			m.bindRootDriverLocked(p.root.ID, tracked, driver)
			g.mu.Lock()
		}
	}
	g.mu.Unlock()
	m.mu.Unlock()
	if err != nil {
		return CommittedSessionView{}, err
	}
	transition.Unlock()
	restore.Unlock()
	locked = false
	if fresh.info.SessionID == fresh.root.ID {
		if tree, ok := snapshotForRoot(m.tree.Snapshot(), subagent.SessionRootID(fresh.root.ID)); ok {
			fresh.info.Session.SetSubagentTree(&tree)
		}
	}
	driver := p.existing[p.info.SessionID]
	handle := &sessionHandle{runtime: p.r, driver: driver, sessionID: p.info.SessionID, agentName: p.info.Binding.AgentName}
	result := CommittedSessionView{SessionHandle: handle, Info: fresh.info, Metadata: handle.Metadata(), Status: driver.Status()}
	p.committed = &result
	return cloneCommittedSessionView(result), nil
}
