package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// preparedSessionView owns detached reservations, never canonical execution.
// The terminal mutex is private to this one confirmed preparation.
type preparedSessionView struct {
	mu           sync.Mutex
	batchMu      sync.Mutex // protects cancellation registration, never storage I/O
	cancelBatch  context.CancelFunc
	r            *LocalRuntime
	ctx          func() context.Context // immutable confirmed preparation lifetime
	cancel       context.CancelFunc
	info         PreparedSessionViewInfo
	root         *session.Session
	snapshot     subagent.Snapshot
	records      []session.ChildRecord
	nodes        []*restoredNode
	reservations []*restoreDriverReservation
	existing     map[string]*sessionDriver
	terminal     bool
	committed    *CommittedSessionView
}

func cloneSessionViewInfo(info PreparedSessionViewInfo) PreparedSessionViewInfo {
	if info.Session != nil {
		info.Session = info.Session.Clone()
	}
	if info.Attach != nil {
		attach := *info.Attach
		if attach.Session != nil {
			attach.Session = attach.Session.Clone()
		}
		info.Attach = &attach
	}
	return info
}

func (p *preparedSessionView) Info() PreparedSessionViewInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return cloneSessionViewInfo(p.info)
}

func (p *preparedSessionView) discard() {
	for _, reservation := range p.reservations {
		reservation.Discard()
	}
	p.reservations = nil
	p.cancel()
}

func (p *preparedSessionView) Abort() {
	// Cancellation is synchronous, but UI callers never wait for Commit's I/O.
	// The registered lifetime callback guarantees cleanup if Info owns mu;
	// an in-flight Commit already owns its deferred cleanup.
	p.cancel()
	// AfterFunc propagation is asynchronous. A returned Abort must also have
	// canceled an in-flight batch, without waiting for Commit's storage I/O.
	p.batchMu.Lock()
	if p.cancelBatch != nil {
		p.cancelBatch()
	}
	p.batchMu.Unlock()
	if !p.mu.TryLock() {
		return
	}
	defer p.mu.Unlock()
	p.abortLocked()
}

func (p *preparedSessionView) abortLocked() {
	if !p.terminal {
		p.terminal = true
		p.discard()
	}
}

// ConfirmedSessionViewInfo performs read-only, confirmed hydration and policy
// checks. It does not reserve, register, restore, persist, or start any owner.
func (v *localSessionRuntimeView) ConfirmedSessionViewInfo(ctx context.Context, id string) (PreparedSessionViewInfo, error) {
	p, err := v.readSessionView(ctx, id)
	if err != nil {
		return PreparedSessionViewInfo{}, err
	}
	return cloneSessionViewInfo(p.info), nil
}

func (v *localSessionRuntimeView) readSessionView(ctx context.Context, id string) (*preparedSessionView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := v.runtime
	if id == "" || r.sessionStore == nil {
		return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: id, Operation: "prepare_view"}
	}
	selected, err := r.sessionStore.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	root := selected
	seen := map[string]bool{id: true}
	// Hydration work is separate from resident-driver and spawn admission: it
	// also counts archived/stopped ancestry and topology nodes. Reuse finite
	// restore capacity; unlimited residency still needs a finite read budget.
	// Zero permits only the selected-row lookup, never ancestor/tree hydration.
	hydrationLimit := r.maxSessions
	if hydrationLimit < 0 {
		hydrationLimit = DefaultSessionResourcePolicy().MaxSessions
	}
	for root.ParentID != "" {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(seen) >= hydrationLimit {
			return nil, &SessionError{Kind: SessionErrorCapacity, SessionID: id, Operation: "restore_ancestry", Reason: SessionErrorReasonLimit, Limit: hydrationLimit}
		}
		parent, err := r.sessionStore.GetSession(ctx, root.ParentID)
		if err != nil {
			return nil, err
		}
		if seen[parent.ID] {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "restore_ancestry"}
		}
		seen[parent.ID] = true
		attrs, parentAttrs := root.AttributesSnapshot(), parent.AttributesSnapshot()
		if source := attrs["docker-agent.actor.source"]; source != "" && source != parentAttrs["docker-agent.actor.source"] {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "restore_source"}
		}
		root = parent
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hydrationLimit == 0 {
		return nil, &SessionError{Kind: SessionErrorCapacity, SessionID: id, Operation: "restore_tree", Reason: SessionErrorReasonLimit, Limit: hydrationLimit}
	}
	bound := root.AttributesSnapshot()[SessionAgentAttribute]
	if bound == "" {
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "restore_binding"}
	}
	if _, err := r.team.Agent(bound); err != nil {
		return nil, err
	}
	root.AgentName = bound
	p := &preparedSessionView{r: r, root: root, existing: make(map[string]*sessionDriver)}
	var legacy subagent.Snapshot
	if r.subagentStore != nil {
		stored, err := r.subagentStore.LoadTree(ctx, root.ID)
		if err != nil {
			return nil, err
		}
		if stored != nil {
			legacy = *stored
		}
	}
	canonical, records, err := r.subagents.canonicalSnapshotWithLimit(ctx, root, legacy, hydrationLimit)
	if err != nil {
		return nil, err
	}
	p.records = records
	if len(canonical.Nodes) != 0 {
		p.snapshot, err = normalizeRestoredSnapshot(canonical, r.sessionDurability(), records...)
		if err != nil {
			return nil, err
		}
		p.nodes, err = r.subagents.preflightRestore(ctx, root, p.snapshot)
		if err != nil {
			return nil, err
		}
	}
	selectedAgent := selected.AttributesSnapshot()[SessionAgentAttribute]
	if selectedAgent == "" {
		return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "restore_binding"}
	}
	selected.AgentName = selectedAgent
	p.info = PreparedSessionViewInfo{SessionID: id, RootSessionID: root.ID, Session: selected, Binding: SessionBinding{AgentName: selectedAgent, Model: selected.AgentModelOverrides[selectedAgent], ParentSessionID: selected.ParentID, Durability: r.sessionDurability()}, WorkingDir: selected.WorkingDir}
	if selected.ParentID != "" {
		for _, entry := range p.nodes {
			if entry.snapshot.Node.SessionID != id {
				continue
			}
			if entry.childSess == nil || entry.childAgent == nil || entry.parentSessionID != selected.ParentID || entry.snapshot.Node.Agent != selectedAgent {
				return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "restore_tree_membership"}
			}
			p.info.Attach = &SubagentAttachInfo{NodeID: entry.snapshot.Node.ID, Agent: selectedAgent, Name: entry.snapshot.Node.DisplayName(), Session: selected.Clone(), ParentSessionID: entry.parentSessionID, ParentAgent: entry.parentSess.AgentName}
			break
		}
		if p.info.Attach == nil {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "restore_tree_membership"}
		}
	}
	return p, nil
}

// sameWorkspace compares physical workspace identity without rewriting stored
// provenance (which may intentionally use a relative path or symlink).
func sameWorkspace(a, b string) bool {
	normalize := func(path string) string {
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		return filepath.Clean(path)
	}
	return normalize(a) == normalize(b)
}

func (v *localSessionRuntimeView) PrepareSessionView(ctx context.Context, id string) (PreparedSessionView, error) {
	p, err := v.readSessionView(ctx, id)
	if err != nil {
		return nil, err
	}
	preparationCtx, cancel := context.WithCancel(ctx)
	p.ctx, p.cancel = func() context.Context { return preparationCtx }, cancel
	if _, live := p.r.sessionDrivers.Lookup(id); !live {
		for _, cwd := range []string{p.root.WorkingDir, p.info.WorkingDir} {
			if cwd != "" && !sameWorkspace(cwd, p.r.workingDir) {
				p.cancel()
				return nil, &SessionError{Kind: SessionErrorWrongSession, SessionID: id, Operation: "view_workspace", Detail: "session view requires its workspace-bound runtime"}
			}
		}
	}
	m, g := p.r.subagents, p.r.sessionDrivers
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	stopped := make(map[string]bool)
	candidates := []*session.Session{p.root}
	for _, entry := range p.nodes {
		if entry.childSess != nil && entry.childAgent != nil && (entry.state != subagent.NodeStopped || entry.childSess.ID == id) {
			stopped[entry.childSess.ID] = entry.state == subagent.NodeStopped
			candidates = append(candidates, entry.childSess)
		}
	}
	for _, candidate := range candidates {
		if err := p.ctx().Err(); err != nil {
			p.discard()
			return nil, err
		}
		var replaces *sessionDriver
		if existing, ok := g.Lookup(candidate.ID); ok {
			existing.mu.Lock()
			identityMatches := existing.sess != nil && existing.sess.ParentID == candidate.ParentID && existing.AgentNameLocked() == candidate.AgentName
			valid := identityMatches && (!existing.stopped || (stopped[candidate.ID] && existing.stoppedView))
			existing.mu.Unlock()
			if !valid && identityMatches && stopped[candidate.ID] && existing.stoppedViewReplaceable() {
				replaces = existing
			} else if !valid {
				p.discard()
				return nil, &SessionError{Kind: SessionErrorConflict, SessionID: candidate.ID, Operation: "prepare_view"}
			}
			if replaces == nil {
				p.existing[candidate.ID] = existing
				continue
			}
		}
		reservation, err := g.prepareRestore(p.ctx(), candidate, replaces)
		if err != nil {
			p.discard()
			return nil, err
		}
		reservation.driver.mu.Lock()
		reservation.driver.viewDormant = true
		if stopped[candidate.ID] {
			reservation.driver.stopped = true
			reservation.driver.stoppedView = true
			reservation.driver.pending = nil
		}
		reservation.driver.mu.Unlock()
		p.reservations = append(p.reservations, reservation)
	}
	context.AfterFunc(preparationCtx, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.abortLocked()
	})
	return p, nil
}

func (p *preparedSessionView) validateRegistryLocked() error {
	g := p.r.sessionDrivers
	if g.closed || p.r.lifetime().Err() != nil {
		return ErrSessionClosed
	}
	for _, id := range append([]string{p.root.ID}, p.info.SessionID) {
		if _, deleted := g.deleted[id]; deleted {
			return ErrSessionStopped
		}
	}
	for id, expected := range p.existing {
		if _, deleted := g.deleted[id]; deleted {
			return ErrSessionStopped
		}
		if g.drivers[id] != expected {
			return &SessionError{Kind: SessionErrorConflict, SessionID: id, Operation: "commit_view"}
		}
		expected.mu.Lock()
		invalidated := expected.stopped && !expected.stoppedView
		expected.mu.Unlock()
		if invalidated {
			return ErrSessionStopped
		}
	}
	for _, reservation := range p.reservations {
		if _, deleted := g.deleted[reservation.id]; deleted {
			return ErrSessionStopped
		}
		if g.reservations[reservation.id] != reservation || g.drivers[reservation.id] != reservation.replaces {
			return &SessionError{Kind: SessionErrorConflict, SessionID: reservation.id, Operation: "commit_view"}
		}
	}
	additional := 0
	for _, reservation := range p.reservations {
		if reservation.replaces == nil {
			additional++
		}
	}
	limit := g.maxSessionsLocked()
	if limit == 0 || (limit > 0 && len(g.drivers)+additional > limit) {
		return &SessionError{Kind: SessionErrorCapacity, Operation: "commit_view", Reason: SessionErrorReasonLimit, Limit: limit}
	}
	return nil
}

func (p *preparedSessionView) canonicalResult() (CommittedSessionView, error) {
	handle, err := p.r.SessionByID(p.info.SessionID)
	if err != nil {
		return CommittedSessionView{}, err
	}
	driver := handle.(*sessionHandle).driver
	driver.mu.Lock()
	info := cloneSessionViewInfo(p.info)
	info.Session = driver.sess.Clone()
	if tree, ok := snapshotForRoot(p.r.subagents.tree.Snapshot(), subagent.SessionRootID(p.root.ID)); ok && info.SessionID == p.root.ID {
		info.Session.SetSubagentTree(&tree)
	}
	info.Binding.AgentName, info.Binding.Model = driver.AgentNameLocked(), driver.modelRef
	info.WorkingDir = driver.sess.WorkingDir
	driver.mu.Unlock()
	if info.Attach != nil {
		info.Attach.Session = info.Session.Clone()
		info.Attach.Agent = info.Binding.AgentName
	}
	return CommittedSessionView{SessionHandle: handle, Info: info}, nil
}

func (p *preparedSessionView) Commit(ctx context.Context) (CommittedSessionView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.committed != nil {
		return CommittedSessionView{SessionHandle: p.committed.SessionHandle, Info: cloneSessionViewInfo(p.committed.Info)}, nil
	}
	if p.terminal {
		return CommittedSessionView{}, context.Canceled
	}
	p.terminal = true
	defer p.discard()
	if err := ctx.Err(); err != nil {
		return CommittedSessionView{}, err
	}
	if err := p.ctx().Err(); err != nil {
		return CommittedSessionView{}, err
	}
	m, g := p.r.subagents, p.r.sessionDrivers
	m.restoreMu.Lock()
	defer m.restoreMu.Unlock()
	m.transitionMu.Lock()
	defer m.transitionMu.Unlock()
	m.mu.Lock()
	for _, record := range p.records {
		if current := m.children[record.Node.ID]; current != nil && current.durable.Revision != record.Revision {
			m.mu.Unlock()
			return CommittedSessionView{}, &SessionError{Kind: SessionErrorConflict, SessionID: record.Node.SessionID, Operation: "commit_view"}
		}
	}
	m.mu.Unlock()
	g.mu.Lock()
	err := p.validateRegistryLocked()
	g.mu.Unlock()
	if err != nil {
		return CommittedSessionView{}, err
	}
	if len(p.reservations) == 0 {
		result, err := p.canonicalResult()
		if err == nil {
			p.committed = &result
		}
		return result, err
	}
	known := make(map[subagent.NodeID]session.ChildRecord, len(p.records))
	for _, record := range p.records {
		known[record.Node.ID] = record
	}
	var admissions []session.ChildAdmission
	for _, entry := range p.nodes {
		if _, exists := known[entry.snapshot.Node.ID]; exists || entry.childSess == nil {
			continue
		}
		row := entry.childSess.OwnSnapshot()
		row.Messages = nil
		node := entry.snapshot.Node
		node.State = entry.state
		record := session.ChildRecord{RootSessionID: p.root.ID, ParentSessionID: entry.parentSessionID, Node: node, Revision: 1}
		admissions = append(admissions, session.ChildAdmission{Child: row, Record: record})
		known[record.Node.ID] = record
	}
	// Validate all topology before durable writes. Missing root is added only
	// at publication; an existing root is retained without timestamp changes.
	var treeNodes []subagent.Node
	rootNode := subagent.Node{ID: subagent.SessionRootID(p.root.ID), Agent: p.root.AgentName, State: subagent.NodeIdle}
	if len(p.snapshot.Nodes) != 0 {
		rootNode = p.snapshot.Nodes[0].Node
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return CommittedSessionView{}, ErrSessionClosed
	}
	_, rootExists := m.tree.Node(rootNode.ID)
	if !rootExists {
		treeNodes = append(treeNodes, rootNode)
	}
	for _, entry := range p.nodes {
		node := entry.snapshot.Node
		if existing, found := m.tree.Node(node.ID); found {
			if existing.SessionID != node.SessionID || existing.Parent != node.Parent || existing.Agent != node.Agent {
				m.mu.Unlock()
				return CommittedSessionView{}, &SessionError{Kind: SessionErrorConflict, SessionID: node.SessionID, Operation: "commit_view"}
			}
			continue
		}
		node.State = entry.state
		treeNodes = append(treeNodes, node)
	}
	m.mu.Unlock()
	if len(admissions) != 0 {
		batch, ok := m.coordination().(session.ChildAdmissionBatchStore)
		if !ok {
			return CommittedSessionView{}, UnsupportedSessionOperation(p.info.SessionID, "restore_legacy_batch")
		}
		if err := p.ctx().Err(); err != nil {
			return CommittedSessionView{}, err
		}
		batchCtx, cancelBatch := context.WithCancel(ctx)
		p.batchMu.Lock()
		// Commit is terminal and serialized by p.mu, so there can be only one
		// registration; Abort never takes p.mu while holding batchMu.
		p.cancelBatch = cancelBatch
		if p.ctx().Err() != nil {
			cancelBatch()
		}
		p.batchMu.Unlock()
		stopPreparation := context.AfterFunc(p.ctx(), cancelBatch)
		err := batch.AdmitChildren(batchCtx, admissions)
		stopPreparation()
		p.batchMu.Lock()
		p.cancelBatch = nil
		cancelBatch()
		p.batchMu.Unlock()
		if err != nil {
			return CommittedSessionView{}, err
		}
	} else if err := ctx.Err(); err != nil {
		return CommittedSessionView{}, err
	} else if err := p.ctx().Err(); err != nil {
		return CommittedSessionView{}, err
	}
	// Preflight-derived terminalization must become canonical before publication.
	var commits []session.ChildCommit
	for _, entry := range p.nodes {
		record, ok := known[entry.snapshot.Node.ID]
		if ok && entry.state == subagent.NodeStopped && record.Node.State != subagent.NodeStopped {
			record.Node.State, record.Node.NeedsAttention, record.Node.WaitingOn = subagent.NodeStopped, false, ""
			commits = append(commits, session.ChildCommit{ExpectedRevision: record.Revision, Record: record})
		}
	}
	if err := m.commitChildren(ctx, commits); err != nil {
		return CommittedSessionView{}, err
	}
	for _, commit := range commits {
		record := commit.Record
		record.Revision++
		known[record.Node.ID] = record
	}
	// A successful legacy batch is durable linearization. Caller cancellation
	// after it cannot revoke admission; shutdown/delete still prevent resurrection.
	m.mu.Lock()
	defer m.mu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if m.closed {
		return CommittedSessionView{}, ErrSessionClosed
	}
	if err := p.validateRegistryLocked(); err != nil {
		return CommittedSessionView{}, err
	}
	if len(admissions) == 0 {
		if err := ctx.Err(); err != nil {
			return CommittedSessionView{}, err
		}
		if err := p.ctx().Err(); err != nil {
			return CommittedSessionView{}, err
		}
	}
	if err := m.tree.AddSubtree(treeNodes); err != nil {
		return CommittedSessionView{}, fmt.Errorf("publish view topology: %w", err)
	}
	if m.sessions[p.root.ID] == nil {
		m.sessions[p.root.ID] = &sessionSubagents{node: rootNode.ID, topLevel: true}
	}
	byID := make(map[string]*sessionDriver, len(p.reservations))
	for _, reservation := range p.reservations {
		byID[reservation.id] = reservation.driver
	}
	for _, entry := range p.nodes {
		node := entry.snapshot.Node
		if _, exists := m.children[node.ID]; exists && byID[node.SessionID] == nil {
			continue
		}
		parentAgent := ""
		if entry.parentSess != nil {
			parentAgent = entry.parentSess.AgentName
		}
		record := &childRecord{name: node.DisplayName(), parentSession: entry.parentSessionID, parentAgentName: parentAgent, sessionID: node.SessionID, agent: entry.childAgent, durable: known[node.ID]}
		if driver := byID[node.SessionID]; driver != nil {
			id := node.ID
			driver.SetPreStartErrorGate(func() error { return m.admitChildRun(id) }, func() { m.abortChildStart(id) })
			record.unwatch = driver.OnStarted(func() { m.markChildRunning(id) })
			m.sessions[node.SessionID] = &sessionSubagents{node: node.ID}
		}
		if old := m.children[node.ID]; old != nil && old.unwatch != nil {
			old.unwatch()
		}
		m.children[node.ID] = record
	}
	for _, reservation := range p.reservations {
		if !reservation.driver.stoppedView {
			reservation.driver.adopt(g.orphans[reservation.id])
		}
		g.drivers[reservation.id] = reservation.driver
		delete(g.orphans, reservation.id)
		delete(g.reservations, reservation.id)
	}
	if rootDriver := g.drivers[p.root.ID]; rootDriver != nil {
		m.bindRootDriverLocked(p.root.ID, m.sessions[p.root.ID], rootDriver)
	}
	// Build the response directly while registry publication is locked: calling
	// SessionByID here would reenter registry.mu.
	driver := g.drivers[p.info.SessionID]
	if driver == nil {
		return CommittedSessionView{}, errors.New("selected view driver was not published")
	}
	driver.mu.Lock()
	info := cloneSessionViewInfo(p.info)
	info.Session = driver.sess.Clone()
	if tree, ok := snapshotForRoot(p.r.subagents.tree.Snapshot(), subagent.SessionRootID(p.root.ID)); ok && info.SessionID == p.root.ID {
		info.Session.SetSubagentTree(&tree)
	}
	info.Binding.AgentName, info.Binding.Model = driver.AgentNameLocked(), driver.modelRef
	info.WorkingDir = driver.sess.WorkingDir
	driver.mu.Unlock()
	if info.Attach != nil {
		info.Attach.Session = info.Session.Clone()
		info.Attach.Agent = info.Binding.AgentName
	}
	result := CommittedSessionView{SessionHandle: &sessionHandle{runtime: p.r, driver: driver, sessionID: info.SessionID, agentName: info.Binding.AgentName}, Info: info}
	p.committed = &result
	return CommittedSessionView{SessionHandle: result.SessionHandle, Info: cloneSessionViewInfo(result.Info)}, nil
}
