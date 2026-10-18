package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/teamdef"
)

// preparedSessionView owns detached reservations, never canonical execution.
// The terminal mutex is private to this one confirmed preparation.
type preparedSessionView struct {
	mu               sync.Mutex
	batchMu          sync.Mutex // protects cancellation registration, never storage I/O
	cancelBatch      context.CancelFunc
	r                *LocalRuntime
	ctx              func() context.Context // immutable confirmed preparation lifetime
	cancel           context.CancelFunc
	info             PreparedSessionViewInfo
	root             *session.Session
	snapshot         subagent.Snapshot
	records          []session.ChildRecord
	nodes            []*restoredNode
	reservations     []*restoreDriverReservation
	existing         map[string]*sessionDriver
	terminal         bool
	committed        *CommittedSessionView
	resident         bool
	historical       bool
	historicalHandle *historicalSessionHandle
	environmentLease *TeamEnvironmentLease
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
	if p.environmentLease != nil {
		_ = p.environmentLease.Release(context.Background())
		p.environmentLease = nil
	}
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
	if p, err := v.readResidentSessionView(ctx, id); p != nil || err != nil {
		return p, err
	}
	return v.readColdSessionView(ctx, id)
}

func (v *localSessionRuntimeView) readColdSessionView(ctx context.Context, id string) (_ *preparedSessionView, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := v.runtime
	var environmentLease *TeamEnvironmentLease
	tm := r.team
	if r.teamEnvironments != nil {
		var err error
		environmentLease, err = r.teamEnvironments.AcquireCurrent()
		if err != nil {
			return nil, err
		}
		tm = environmentLease.Environment().Team()
		ctx = environmentScopedContext(ctx, environmentLease)
		defer func() {
			if environmentLease != nil {
				_ = environmentLease.Release(context.Background())
			}
		}()
	}
	if id == "" || r.sessionStore == nil {
		return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: id, Operation: "prepare_view"}
	}
	selectedMeta, err := session.ReadSessionMetadata(ctx, r.sessionStore, id)
	if err != nil {
		return nil, err
	}
	root := selectedMeta
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
		parent, err := session.ReadSessionMetadata(ctx, r.sessionStore, root.ParentID)
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
	rootAgent, _, rootIdentityOK := identityForBinding(tm, root, SessionAgentAttribute, SessionAgentIDAttribute)
	if bound == "" || !rootIdentityOK {
		// Legacy unbound rows and stale identities are viewable only as historical transcripts.
		return v.prepareHistoricalSessionView(ctx, selectedMeta)
	}
	selectedAgent := selectedMeta.AttributesSnapshot()[SessionAgentAttribute]
	_, _, selectedIdentityOK := identityForBinding(tm, selectedMeta, SessionAgentAttribute, SessionAgentIDAttribute)
	if selectedAgent == "" || !selectedIdentityOK {
		return v.prepareHistoricalSessionView(ctx, selectedMeta)
	}
	selectedActive := selectedMeta.AgentName
	if selectedMeta.AgentID != "" {
		_, currentName, ok := tm.ResolveIdentity(selectedMeta.AgentID)
		if !ok {
			return v.prepareHistoricalSessionView(ctx, selectedMeta)
		}
		selectedActive = currentName
	} else if selectedActive == "" {
		selectedActive = selectedAgent
	}
	_, _, activeOK := resolveAgentIdentity(tm, selectedMeta.AgentID, selectedActive)
	if !activeOK {
		return v.prepareHistoricalSessionView(ctx, selectedMeta)
	}
	rootCreationName := rootAgent.Name()
	root, err = session.ReadSessionMetadata(ctx, r.sessionStore, root.ID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rootActive, rootActiveName, rootActiveOK := resolveActiveAgentForTeam(tm, root)
	if !rootActiveOK || rootActive == nil {
		return v.prepareHistoricalSessionView(ctx, selectedMeta)
	}
	root.AgentName, root.AgentID = rootActiveName, configuredAgentID(tm, rootActiveName)
	var selected *session.Session
	if root.ID == id {
		// A root restore owns its embedded-child projection and accepted FIFO;
		// retain the established full-session compatibility contract here.
		selected, err = r.sessionStore.GetSession(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		root = selected
	} else if reader, ok := r.sessionStore.(session.OwnHistoryStore); ok {
		selected, err = reader.GetSessionOwnHistory(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	} else {
		// Legacy executable restore stores may expose only GetSession. Historical
		// transcript views deliberately remain stricter and never recurse here.
		selected, err = r.sessionStore.GetSession(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	// Ordinary restoration needs the complete root transcript and topology; only
	// this validated current-team path pays for recursive history hydration.
	if root.ID != selected.ID {
		root, err = r.sessionStore.GetSession(ctx, root.ID)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if root.ID != selected.ID {
		rootActive, rootActiveName, rootActiveOK = resolveActiveAgentForTeam(tm, root)
		if !rootActiveOK || rootActive == nil {
			return v.prepareHistoricalSessionView(ctx, selectedMeta)
		}
		root.AgentName, root.AgentID = rootActiveName, configuredAgentID(tm, rootActiveName)
	}
	_ = rootCreationName
	selected.AgentName = selectedActive
	selected.AgentID = configuredAgentID(tm, selectedActive)
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
	p.snapshot, err = normalizeRestoredSnapshot(canonical, r.sessionDurability(), records...)
	if err != nil {
		return nil, err
	}
	if len(canonical.Nodes) != 0 {
		p.nodes, err = r.subagents.preflightRestoreForTeam(ctx, root, p.snapshot, tm)
		if err != nil {
			return nil, err
		}
	}
	selected.AgentName = selectedActive
	p.info = PreparedSessionViewInfo{ActiveAgentName: selectedActive, SessionID: id, RootSessionID: root.ID, RootWorkingDir: root.WorkingDir, RootBinding: SessionBinding{AgentName: bound}, Session: selected, Binding: SessionBinding{AgentName: selectedAgent, Model: selected.AgentModelOverrides[selectedActive], ParentSessionID: selected.ParentID, Durability: r.sessionDurability()}, WorkingDir: selected.WorkingDir}
	if selected.ParentID != "" {
		for _, entry := range p.nodes {
			if entry.snapshot.Node.SessionID != id {
				continue
			}
			if entry.childSess == nil || entry.childAgent == nil || entry.parentSessionID != selected.ParentID || entry.snapshot.Node.Agent != selectedAgent {
				return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "restore_tree_membership"}
			}
			p.info.Attach = &SubagentAttachInfo{NodeID: entry.snapshot.Node.ID, Agent: selectedActive, Name: entry.snapshot.Node.DisplayName(), Session: selected.Clone(), ParentSessionID: entry.parentSessionID, ParentAgent: entry.parentSess.AgentName}
			break
		}
		if p.info.Attach == nil {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "restore_tree_membership"}
		}
	}
	p.environmentLease = environmentLease
	environmentLease = nil
	return p, nil
}

// HistoricalSessionView returns a transcript-only handle without publishing a
// session driver, restoring a tree, or loading sibling transcripts.
func (v *localSessionRuntimeView) HistoricalSessionView(ctx context.Context, id string) (SessionHandle, PreparedSessionViewInfo, error) {
	if v.runtime.sessionStore == nil {
		return nil, PreparedSessionViewInfo{}, &SessionError{Kind: SessionErrorNotFound, SessionID: id, Operation: "historical_view"}
	}
	meta, err := session.ReadSessionMetadata(ctx, v.runtime.sessionStore, id)
	if err != nil {
		return nil, PreparedSessionViewInfo{}, err
	}
	prepared, err := v.prepareHistoricalSessionView(ctx, meta)
	if err != nil {
		return nil, PreparedSessionViewInfo{}, err
	}
	return prepared.historicalHandle, prepared.Info(), nil
}

func (v *localSessionRuntimeView) prepareHistoricalSessionView(ctx context.Context, selectedMeta *session.Session) (*preparedSessionView, error) {
	if selectedMeta == nil {
		return nil, session.ErrNotFound
	}
	root, err := session.ReadSessionMetadata(ctx, v.runtime.sessionStore, selectedMeta.ID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{root.ID: true}
	for depth := 0; root.ParentID != ""; depth++ {
		if depth >= 32 || seen[root.ParentID] {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: selectedMeta.ID, Operation: "restore_ancestry"}
		}
		parent, err := session.ReadSessionMetadata(ctx, v.runtime.sessionStore, root.ParentID)
		if err != nil {
			return nil, err
		}
		if a, b := root.AttributesSnapshot(), parent.AttributesSnapshot(); a["docker-agent.actor.source"] != "" && a["docker-agent.actor.source"] != b["docker-agent.actor.source"] {
			return nil, &SessionError{Kind: SessionErrorWrongSession, SessionID: selectedMeta.ID, Operation: "restore_source"}
		}
		seen[parent.ID] = true
		root = parent
	}
	selected, err := session.ReadSessionOwnHistory(ctx, v.runtime.sessionStore, selectedMeta.ID)
	if err != nil {
		return nil, err
	}
	attrs := selectedMeta.AttributesSnapshot()
	recorded := attrs[SessionAgentAttribute]
	if recorded == "" {
		recorded = selectedMeta.AgentName
	}
	if selected.AgentName == "" {
		selected.AgentName = recorded
	}
	reason := historicalDefinitionMissing
	var definition *teamdef.AgentDefinition
	if recorded != "" {
		if revisions, ok := v.runtime.sessionStore.(session.TeamRevisionStore); ok {
			revision, revErr := revisions.LatestTeamRevision(ctx, selected.ID)
			if revErr == nil {
				entry, entryErr := revisions.TeamAgentDefinition(ctx, revision.AgentDigest)
				if entryErr == nil {
					var saved teamdef.AgentDefinition
					if err := json.Unmarshal(entry.Payload, &saved); err != nil {
						return nil, fmt.Errorf("decode historical agent definition: %w", err)
					}
					if saved.FormatVersion != teamdef.DefinitionFormatVersion || saved.TeamID != revision.TeamID || saved.AgentID != revision.AgentID || saved.AgentKey != revision.AgentKey || saved.Name != revision.AgentName {
						return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: selected.ID, Operation: "historical_definition"}
					}
					definition = &saved
					reason = historicalAgentUnavailable
					if len(saved.Unavailable) > 0 {
						reason = "partial_definition: " + strings.Join(saved.Unavailable, ",")
					}
				} else if !errors.Is(entryErr, session.ErrTeamDefinitionNotFound) {
					return nil, entryErr
				}
			} else if !errors.Is(revErr, session.ErrTeamDefinitionNotFound) {
				return nil, revErr
			}
		}
	}
	var attach *SubagentAttachInfo
	var topology *subagent.Snapshot
	if v.runtime.subagentStore != nil {
		storedTree, err := v.runtime.subagentStore.LoadTree(ctx, root.ID)
		if err != nil {
			return nil, err
		}
		if storedTree != nil {
			if storedTree.Root != subagent.SessionRootID(root.ID) || len(storedTree.Nodes) != 1 || storedTree.Nodes[0].Node.ID != storedTree.Root {
				return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: selected.ID, Operation: "historical_tree"}
			}
			topology = storedTree
		}
	}
	if selected.ParentID != "" {
		reader, ok := v.runtime.sessionStore.(session.ChildRecordReader)
		if !ok {
			return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: selected.ID, Operation: "restore_tree_membership"}
		}
		record, err := reader.LoadChild(ctx, selected.ID)
		if err != nil {
			return nil, err
		}
		if record.RootSessionID != root.ID || record.ParentSessionID != selected.ParentID || record.Node.SessionID != selected.ID || recorded != "" && record.Node.Agent != recorded {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: selected.ID, Operation: "restore_tree_membership"}
		}
		if topology == nil {
			rootNode := subagent.Node{ID: subagent.SessionRootID(root.ID), Agent: root.AttributesSnapshot()[SessionAgentAttribute], State: subagent.NodeIdle}
			topology = &subagent.Snapshot{Version: subagent.SnapshotVersion, Root: rootNode.ID, Nodes: []subagent.NodeSnapshot{{Node: rootNode, Children: []subagent.NodeSnapshot{{Node: record.Node}}}}}
		}
		node, found := subtreeForSession(*topology, selected.ID)
		if !found || node.Node.ID != record.Node.ID || node.Node.Parent != record.Node.Parent || node.Node.Agent != record.Node.Agent {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: selected.ID, Operation: "restore_tree_membership"}
		}
		parent, err := session.ReadSessionMetadata(ctx, v.runtime.sessionStore, selected.ParentID)
		if err != nil {
			return nil, err
		}
		attach = &SubagentAttachInfo{NodeID: record.Node.ID, Name: record.Node.DisplayName(), Agent: record.Node.Agent, Session: selected.Clone(), ParentSessionID: selected.ParentID, ParentAgent: parent.AgentName}
		topology = &subagent.Snapshot{Version: topology.Version, Durability: topology.Durability, Root: node.Node.ID, Nodes: []subagent.NodeSnapshot{node}}
	}
	selected.SetSubagentTree(topology)
	info := PreparedSessionViewInfo{ActiveAgentName: recorded, SessionID: selected.ID, RootSessionID: root.ID, RootWorkingDir: root.WorkingDir, RootBinding: SessionBinding{AgentName: root.AttributesSnapshot()[SessionAgentAttribute]}, Session: selected, WorkingDir: selected.WorkingDir, Binding: SessionBinding{AgentName: recorded, ParentSessionID: selected.ParentID, Durability: v.runtime.sessionDurability()}, Attach: attach, Historical: true, HistoryReason: reason}
	metadata := SessionMetadata{SessionID: selected.ID, AgentName: recorded, Historical: true, ExecutionAvailable: false, HistoryReason: reason, Capabilities: SessionCapabilities{Historical: true, ExecutionAvailable: false}}
	handle := newHistoricalSessionHandle(selected.ID, selected, metadata, definition)
	return &preparedSessionView{r: v.runtime, info: info, root: root, historical: true, historicalHandle: handle, existing: map[string]*sessionDriver{}}, nil
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
	if p.environmentLease != nil {
		preparationCtx = environmentScopedContext(preparationCtx, p.environmentLease)
	}
	p.ctx, p.cancel = func() context.Context { return preparationCtx }, cancel
	if _, live := p.r.sessionDrivers.Lookup(id); !live {
		for _, cwd := range []string{p.root.WorkingDir, p.info.WorkingDir} {
			if cwd != "" && !sameWorkspace(cwd, p.r.workingDir) {
				p.abortLocked()
				return nil, &SessionError{Kind: SessionErrorWrongSession, SessionID: id, Operation: "view_workspace", Detail: "session view requires its workspace-bound runtime"}
			}
		}
	}
	m, g := p.r.subagents, p.r.sessionDrivers
	if p.resident {
		context.AfterFunc(preparationCtx, func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.abortLocked()
		})
		return p, nil
	}
	restore := m.transition("restore:" + p.root.ID)
	restore.Lock()
	defer restore.Unlock()
	transition := m.transition(p.root.ID)
	transition.Lock()
	defer transition.Unlock()
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
			projection := existing.registrySnapshot()
			identityMatches := existing.identityParent == candidate.ParentID && projection.agentName == candidate.AgentName
			valid := identityMatches && (!projection.stopped || (stopped[candidate.ID] && projection.stoppedView))
			if !valid && identityMatches && stopped[candidate.ID] && projection.replaceable && driverDrained(existing) {
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
		if err := reservation.driver.ownerCall(p.ctx(), func() error {
			reservation.driver.viewDormant = true
			if stopped[candidate.ID] {
				reservation.driver.stopped = true
				reservation.driver.stoppedView = true
				reservation.driver.pending = nil
			}
			return nil
		}); err != nil {
			reservation.Discard()
			p.discard()
			return nil, err
		}
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
		projection := expected.registrySnapshot()
		if projection.stopped && !projection.stoppedView {
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
	return nil
}

func (p *preparedSessionView) Commit(ctx context.Context) (CommittedSessionView, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.committed != nil {
		return cloneCommittedSessionView(*p.committed), nil
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
	if p.historical {
		result := CommittedSessionView{SessionHandle: p.historicalHandle, Info: cloneSessionViewInfo(p.info), Metadata: p.historicalHandle.Metadata(), Status: SessionStatus{SessionID: p.info.SessionID, AgentName: p.info.ActiveAgentName, State: SessionStateSettled}}
		p.committed = &result
		return cloneCommittedSessionView(result), nil
	}
	if p.resident {
		return p.commitResident(ctx)
	}
	m, g := p.r.subagents, p.r.sessionDrivers
	restore := m.transition("restore:" + p.root.ID)
	restore.Lock()
	publicationLocked := true
	defer func() {
		if publicationLocked {
			restore.Unlock()
		}
	}()
	transition := m.transition(p.root.ID)
	transition.Lock()
	defer func() {
		if publicationLocked {
			transition.Unlock()
		}
	}()
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
	defer func() {
		if publicationLocked {
			m.mu.Unlock()
		}
	}()
	g.mu.Lock()
	defer func() {
		if publicationLocked {
			g.mu.Unlock()
		}
	}()
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
			g.mu.Unlock()
			driver.SetPreStartErrorGate(func() error { return m.admitChildRun(id) }, func() { m.abortChildStart(id) })
			record.unwatch = driver.OnStarted(func() { m.markChildRunning(id) })
			g.mu.Lock()
			m.sessions[node.SessionID] = &sessionSubagents{node: node.ID}
		}
		if old := m.children[node.ID]; old != nil && old.unwatch != nil {
			g.mu.Unlock()
			old.unwatch()
			g.mu.Lock()
		}
		m.children[node.ID] = record
	}
	for _, reservation := range p.reservations {
		g.drivers[reservation.id] = reservation.driver
		delete(g.reservations, reservation.id)
	}
	if rootDriver := g.drivers[p.root.ID]; rootDriver != nil {
		g.mu.Unlock()
		m.bindRootDriverLocked(p.root.ID, m.sessions[p.root.ID], rootDriver)
		g.mu.Lock()
	}
	// Build the response directly while registry publication is locked: calling
	// SessionByID here would reenter registry.mu.
	driver := g.drivers[p.info.SessionID]
	if driver == nil {
		return CommittedSessionView{}, errors.New("selected view driver was not published")
	}
	g.mu.Unlock()
	info := p.info
	info.Session = driver.session()
	if info.Session == nil {
		g.mu.Lock()
		return CommittedSessionView{}, ErrSessionClosed
	}
	handle := &sessionHandle{runtime: p.r, driver: driver, sessionID: info.SessionID, agentName: info.Binding.AgentName}
	_, modelRef, _ := driver.ModelBindingSnapshot()
	status := driver.Status()
	g.mu.Lock()
	if info.SessionID == p.root.ID {
		if tree, ok := snapshotForRoot(p.r.subagents.tree.Snapshot(), subagent.SessionRootID(p.root.ID)); ok {
			info.Session.SetSubagentTree(&tree)
		}
	}
	info.ActiveAgentName = info.Session.AgentName
	info.Binding.Model = modelRef
	info.WorkingDir = info.Session.WorkingDir
	if info.Attach != nil {
		// Copy attach metadata without cloning the stale session it replaces.
		attach := *info.Attach
		info.Attach = &attach
		info.Attach.Session = info.Session.Clone()
		info.Attach.Agent = info.ActiveAgentName
	}
	// Catalog discovery may perform I/O; publication is complete before metadata hydration.
	g.mu.Unlock()
	m.mu.Unlock()
	transition.Unlock()
	restore.Unlock()
	publicationLocked = false
	for _, reservation := range p.reservations {
		if reservation != nil {
			reservation.driver.WakePending()
		}
	}
	metadata := handle.Metadata()
	result := CommittedSessionView{SessionHandle: handle, Info: info, Metadata: metadata, Status: status}
	p.committed = &result
	return cloneCommittedSessionView(result), nil
}

// OpenSessionView acquires a confirmed, ready view without starting execution.
// Abort releases only the preparation, never a committed owner.
func OpenSessionView(ctx context.Context, sessions SessionRuntime, id string) (CommittedSessionView, error) {
	preparer, ok := sessions.(SessionViewPreparer)
	if !ok {
		return CommittedSessionView{}, UnsupportedSessionOperation(id, "prepare_view")
	}
	prepared, err := preparer.PrepareSessionView(ctx, id)
	if err != nil {
		return CommittedSessionView{}, err
	}
	defer prepared.Abort()
	return prepared.Commit(ctx)
}

// RestoreSessionView delegates to canonical ready acquisition.
func RestoreSessionView(ctx context.Context, sessions SessionRuntime, id string) (CommittedSessionView, error) {
	return OpenSessionView(ctx, sessions, id)
}

func cloneCommittedSessionView(view CommittedSessionView) CommittedSessionView {
	view.Info = cloneSessionViewInfo(view.Info)
	view.Metadata.ThinkingLevels = slices.Clone(view.Metadata.ThinkingLevels)
	view.Metadata.Capabilities.AvailableModels = slices.Clone(view.Metadata.Capabilities.AvailableModels)
	return view
}
