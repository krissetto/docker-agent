package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
)

// SubagentTarget is a portable navigation target. SessionID is canonical; the
// node identity is presentation metadata and never required to open a view.
type SubagentTarget struct {
	NodeID    subagent.NodeID
	SessionID string
	Name      string
	Agent     string
}

// TreeAttention is one descendant session waiting on a human.
type TreeAttention struct {
	SubagentTarget

	WaitingOn    string
	OccurrenceID string
	Error        string
}

// Matches runtime.sessionDriver.refreshAttention so local and remote trees read alike.
const (
	waitingOnApproval = "approve tool"
	waitingOnAnswer   = "answer question"
)

// LiveRuntimeTree reports whether topology comes from an in-process tree
// shared by every view of the runtime, rather than this view's observation.
func (a *App) LiveRuntimeTree() bool {
	if handle := a.SessionHandle(); handle != nil && !runtime.IsLocalSessionHandle(handle) && handle.Metadata().Historical {
		return false
	}
	rt, ok := a.runtime.(subagentRuntime)
	return ok && rt.SubagentTree() != nil
}

// SubagentTreeSnapshot returns the freshest portable topology for this view:
// the shared live tree when in-process, otherwise the canonical session tree
// overlaid with this view's tree observation (remote clients).
func (a *App) SubagentTreeSnapshot() *subagent.Snapshot {
	if rt, ok := a.runtime.(subagentRuntime); ok && rt.SubagentTree() != nil && a.LiveRuntimeTree() {
		snapshot := rt.SubagentTree().Snapshot()
		return &snapshot
	}
	sess := a.Session()
	if sess == nil {
		return nil
	}
	base := sess.GetSubagentTree()
	a.treeMu.Lock()
	watch := a.treeWatch
	a.treeMu.Unlock()
	if watch == nil || watch.rootSessionID != sess.ID {
		return base
	}
	return watch.merged(base)
}

// ResolveSubagentTarget maps a node ID or exact session ID to its canonical
// session. Display names and partial IDs are never lookup keys.
func (a *App) ResolveSubagentTarget(id string) (SubagentTarget, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return SubagentTarget{}, false
	}
	snapshot := a.SubagentTreeSnapshot()
	if snapshot == nil {
		snapshot = &subagent.Snapshot{}
	}
	if sess := a.Session(); sess != nil && sess.ID == id {
		target := SubagentTarget{SessionID: id, Agent: a.Binding().AgentName}
		if attached := a.AttachedSubagent(); attached != nil {
			target.NodeID, target.Name, target.Agent = attached.NodeID, attached.Name, attached.Agent
		}
		return target, true
	}
	return FindSubagentTarget(*snapshot, id)
}

// FindSubagentTarget resolves an exact node or session ID within snapshot, for
// views that render a tree they received as an event.
func FindSubagentTarget(snapshot subagent.Snapshot, id string) (SubagentTarget, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return SubagentTarget{}, false
	}
	node, ok := findNode(snapshot.Nodes, func(n subagent.Node) bool {
		return n.ID == subagent.NodeID(id) || n.SessionID == id
	})
	if !ok || node.SessionID == "" {
		return SubagentTarget{}, false
	}
	return targetOf(node), true
}

// SubagentNodeForSession maps a session to its tree node. In-process owners
// answer from their index without copying the whole tree.
func (a *App) SubagentNodeForSession(sessionID string) (subagent.NodeID, bool) {
	if index, ok := a.runtime.(interface {
		SubagentNodeForSession(sessionID string) (subagent.NodeID, bool)
	}); ok && a.LiveRuntimeTree() {
		return index.SubagentNodeForSession(sessionID)
	}
	snapshot := a.SubagentTreeSnapshot()
	if snapshot == nil || sessionID == "" {
		return "", false
	}
	node, ok := findNode(snapshot.Nodes, func(n subagent.Node) bool { return n.SessionID == sessionID && n.Parent != "" })
	return node.ID, ok
}

// SubagentChildCount counts direct children of nodeID in the portable tree.
func (a *App) SubagentChildCount(nodeID subagent.NodeID) (int, bool) {
	if rt, ok := a.runtime.(subagentRuntime); ok && rt.SubagentTree() != nil && a.LiveRuntimeTree() {
		if count, found := rt.SubagentTree().ChildCount(nodeID); found {
			return count, true
		}
	}
	// An initial live tree may not yet contain a restored session's root.
	snapshot := a.SubagentTreeSnapshot()
	if rt, ok := a.runtime.(subagentRuntime); ok && rt.SubagentTree() != nil && a.LiveRuntimeTree() {
		if sess := a.Session(); sess != nil {
			snapshot = sess.GetSubagentTree()
		}
	}
	if snapshot == nil {
		return 0, false
	}
	node, ok := findSnapshot(snapshot.Nodes, nodeID)
	return len(node.Children), ok
}

// DescendantAttention lists sessions below this view waiting on a human. It
// is derived from the same tree topology for in-process and remote runtimes.
func (a *App) DescendantAttention() []TreeAttention {
	sess := a.Session()
	snapshot := a.SubagentTreeSnapshot()
	if sess == nil || snapshot == nil {
		return nil
	}
	return DescendantAttention(*snapshot, sess.ID)
}

// DescendantAttention lists strict descendants of sessionID needing attention.
func DescendantAttention(snapshot subagent.Snapshot, sessionID string) []TreeAttention {
	root, ok := findNode(snapshot.Nodes, func(n subagent.Node) bool {
		return n.SessionID == sessionID || n.ID == subagent.SessionRootID(sessionID)
	})
	if !ok {
		return nil
	}
	var subtree subagent.NodeSnapshot
	if found, ok := findSnapshot(snapshot.Nodes, root.ID); ok {
		subtree = found
	}
	var out []TreeAttention
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, child := range nodes {
			node := child.Node
			if node.NeedsAttention && node.SessionID != "" && node.State != subagent.NodeStopped {
				waitingOn := node.WaitingOn
				if node.State == subagent.NodeFailed && node.Error != "" {
					waitingOn = "failed"
				}
				if waitingOn == waitingOnApproval || waitingOn == waitingOnAnswer || waitingOn == "failed" && node.Error != "" {
					ids := subagent.AttentionOccurrences(node.AttentionID)
					if len(ids) == 0 {
						ids = []string{""}
					}
					for _, id := range ids {
						out = append(out, TreeAttention{SubagentTarget: targetOf(node), WaitingOn: waitingOn, Error: node.Error, OccurrenceID: id})
					}
				}
			}
			walk(child.Children)
		}
	}
	walk(subtree.Children)
	return out
}

// CanStopSubtree reports the portable stop-tree capability of this view's owner.
func (a *App) CanStopSubtree() bool {
	h := a.SessionHandle()
	if h == nil {
		return false
	}
	_, ok := h.(runtime.SessionTreeController)
	return ok && h.Metadata().Capabilities.StopSubtree
}

// StopSubtree asks the canonical session owner to stop and drain target and
// its descendants, preserving their transcripts; any session, including this
// view's own root, is delegated to the owner's policy.
func (a *App) StopSubtree(ctx context.Context, target SubagentTarget) error {
	if !a.CanStopSubtree() || a.sessions == nil {
		return runtime.UnsupportedSessionOperation(target.SessionID, "stop_subtree")
	}
	if target.SessionID == "" {
		return errors.New("this subagent has no session to stop")
	}
	child, err := a.sessions.SessionByID(target.SessionID)
	if err != nil {
		return err
	}
	if hydrator, ok := child.(runtime.Hydrator); ok {
		if err := hydrator.Hydrate(ctx); err != nil {
			return err
		}
	}
	controller, ok := child.(runtime.SessionTreeController)
	if !ok || !child.Metadata().Capabilities.StopSubtree {
		return runtime.UnsupportedSessionOperation(target.SessionID, "stop_subtree")
	}
	return controller.StopSubtree(ctx)
}

func targetOf(n subagent.Node) SubagentTarget {
	return SubagentTarget{NodeID: n.ID, SessionID: n.SessionID, Name: n.DisplayName(), Agent: n.Agent}
}

func findNode(nodes []subagent.NodeSnapshot, match func(subagent.Node) bool) (subagent.Node, bool) {
	for _, node := range nodes {
		if match(node.Node) {
			return node.Node, true
		}
		if found, ok := findNode(node.Children, match); ok {
			return found, true
		}
	}
	return subagent.Node{}, false
}

func findSnapshot(nodes []subagent.NodeSnapshot, id subagent.NodeID) (subagent.NodeSnapshot, bool) {
	for _, node := range nodes {
		if node.Node.ID == id {
			return node, true
		}
		if found, ok := findSnapshot(node.Children, id); ok {
			return found, true
		}
	}
	return subagent.NodeSnapshot{}, false
}

// treeWatch is the portable descendant projection for views without an
// in-process tree. It retains topology, own cost, run state and open interactions.
type treeWatch struct {
	rootSessionID string

	mu       sync.Mutex
	rootTree *subagent.Snapshot // the root's tree as of the latest tree baseline
	sessions map[string]*watchedSession
	created  map[string]*runtime.SubagentCreatedEvent
	order    []string
}

type watchedSession struct {
	agent                   string
	ownCost                 float64
	running                 bool
	backgroundProcesses     int
	backgroundProcessesSeen bool
	known                   bool
	seen                    bool // events applied before this session's snapshot arrived
	interactions            map[string]runtime.InteractionKind
	resolved                map[string]bool
	seeded                  bool
	seedKey                 seedKey
}

// seedKey identifies the usage last published for a descendant, so an unchanged
// descendant is not looked up again.
type seedKey struct {
	input, output int64
	ownCost       float64
}

func newTreeWatch(rootSessionID string) *treeWatch {
	return &treeWatch{rootSessionID: rootSessionID, sessions: map[string]*watchedSession{}, created: map[string]*runtime.SubagentCreatedEvent{}}
}

func (w *treeWatch) session(id string) *watchedSession {
	s := w.sessions[id]
	if s == nil {
		s = &watchedSession{interactions: map[string]runtime.InteractionKind{}, resolved: map[string]bool{}}
		w.sessions[id] = s
	}
	return s
}

// reset discards observed state at a fresh tree baseline. Spawn edges survive:
// they are immutable and the baseline replays none of them.
func (w *treeWatch) reset(snapshots []runtime.SessionSnapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	previous := w.sessions
	w.sessions = map[string]*watchedSession{}
	for id, s := range previous {
		w.session(id).resolved = s.resolved
	}
	for _, snapshot := range snapshots {
		w.addLocked(snapshot)
	}
}

func (w *treeWatch) add(snapshot runtime.SessionSnapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.addLocked(snapshot)
}

func (w *treeWatch) addLocked(snapshot runtime.SessionSnapshot) {
	id := snapshot.Status.SessionID
	if id == "" && snapshot.Session != nil {
		id = snapshot.Session.ID
	}
	if id == "" {
		return
	}
	if id == w.rootSessionID && snapshot.Session != nil {
		if tree := snapshot.Session.GetSubagentTree(); tree != nil {
			w.rootTree = tree
		}
	}
	s := w.session(id)
	s.known = true
	if snapshot.Session != nil {
		s.ownCost = max(s.ownCost, snapshot.Session.OwnCost())
	}
	if !s.backgroundProcessesSeen {
		s.backgroundProcesses = 0
		for _, process := range snapshot.BackgroundProcesses {
			if process.SessionID == id && process.State == tools.BackgroundProcessRunning {
				s.backgroundProcesses++
			}
		}
		s.backgroundProcessesSeen = true
	}
	s.agent = snapshot.Status.AgentName
	if s.agent == "" && snapshot.Session != nil {
		s.agent = snapshot.Session.AgentName
	}
	// SessionsAdded and Events are separate channels: events consumed first
	// are newer than this snapshot and win over it.
	if !s.seen {
		s.running = snapshot.Status.State == runtime.SessionStateRunning || snapshot.Status.State == runtime.SessionStateQueued || snapshot.Status.State == runtime.SessionStateCancelling
	}
	for _, interaction := range snapshot.Interactions {
		key := interactionSnapshotKey(interaction)
		if key.InteractionID != "" && !s.resolved[key.InteractionID] {
			s.interactions[key.InteractionID] = interactionKind(interaction.Kind, interaction.Event)
		}
	}
}

// apply folds one descendant event; it reports whether topology, run state or
// attention changed. Streaming deltas are ignored without allocation.
func (w *treeWatch) apply(envelope runtime.SessionEvent) bool {
	sessionID := envelope.SessionID
	w.mu.Lock()
	defer w.mu.Unlock()
	switch e := envelope.Event.(type) {
	case *runtime.TokenUsageEvent:
		// Legacy Cost can contain embedded descendants. Canonical node metrics
		// and snapshot own costs remain the fallback for older servers.
		if e.Usage == nil || e.Usage.OwnCost == nil || sessionID == "" {
			return false
		}
		s := w.session(sessionID)
		cost := max(s.ownCost, *e.Usage.OwnCost)
		changed := cost != s.ownCost
		s.ownCost = cost
		return changed
	case *runtime.BackgroundProcessesEvent:
		if e.SessionID == "" || e.SessionID != sessionID {
			return false
		}
		s := w.session(sessionID)
		count := 0
		for _, process := range e.Processes {
			if process.SessionID == sessionID && process.State == tools.BackgroundProcessRunning {
				count++
			}
		}
		changed := !s.backgroundProcessesSeen || count != s.backgroundProcesses
		s.backgroundProcesses = count
		s.backgroundProcessesSeen = true
		return changed
	case *runtime.SubagentTreeEvent:
		if envelope.SessionID != w.rootSessionID {
			return false
		}
		root, ok := findSnapshot(e.Snapshot.Nodes, e.Snapshot.Root)
		if !ok || (root.Node.SessionID != w.rootSessionID && root.Node.ID != subagent.SessionRootID(w.rootSessionID)) {
			return false
		}
		tree := e.Snapshot
		tree.Nodes = cloneNodes(tree.Nodes)
		w.rootTree = &tree
		return true
	case *runtime.SubagentCreatedEvent:
		if e.ChildSessionID == "" || w.created[e.ChildSessionID] != nil {
			return false
		}
		w.created[e.ChildSessionID] = e
		w.order = append(w.order, e.ChildSessionID)
		return true
	case *runtime.StreamStartedEvent:
		s := w.session(sessionID)
		s.seen = true
		changed := !s.running
		s.running = true
		return changed
	case *runtime.StreamStoppedEvent:
		s := w.session(sessionID)
		s.seen = true
		changed := s.running
		s.running = false
		return changed
	case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent:
		key := InteractionIdentity(envelope.Event)
		if key.SessionID == "" {
			key.SessionID = sessionID
		}
		if key.InteractionID == "" {
			key.InteractionID = envelope.InteractionID
		}
		if key.InteractionID == "" {
			return false
		}
		s := w.session(key.SessionID)
		if _, exists := s.interactions[key.InteractionID]; exists || s.resolved[key.InteractionID] {
			return false
		}
		s.interactions[key.InteractionID] = interactionKind("", e)
		return true
	case *runtime.InteractionResolvedEvent:
		id := e.SessionID
		if id == "" {
			id = sessionID
		}
		s := w.session(id)
		s.resolved[e.InteractionID] = true
		if _, exists := s.interactions[e.InteractionID]; !exists {
			return false
		}
		delete(s.interactions, e.InteractionID)
		return true
	}
	return false
}

func interactionKind(kind runtime.InteractionKind, event runtime.Event) runtime.InteractionKind {
	if kind != "" {
		return kind
	}
	if _, ok := event.(*runtime.ElicitationRequestEvent); ok {
		return runtime.InteractionElicitation
	}
	return runtime.InteractionConfirmation
}

func (s *watchedSession) waitingOn() string {
	waiting := ""
	for _, kind := range s.interactions {
		if kind != runtime.InteractionElicitation {
			return waitingOnApproval
		}
		waiting = waitingOnAnswer
	}
	return waiting
}

// merged overlays observed descendants onto the canonical base tree.
func (w *treeWatch) merged(base *subagent.Snapshot) *subagent.Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out subagent.Snapshot
	if base != nil {
		out = *base
		out.Nodes = cloneNodes(base.Nodes)
	}
	if w.rootTree != nil {
		out = *w.rootTree
		out.Nodes = cloneNodes(w.rootTree.Nodes)
		if base != nil {
			out.Nodes = graftMissing(out.Nodes, base.Nodes)
		}
	}
	rootID := subagent.SessionRootID(w.rootSessionID)
	if len(w.order) > 0 {
		if _, ok := findNode(out.Nodes, func(n subagent.Node) bool { return n.SessionID == w.rootSessionID || n.ID == rootID }); !ok {
			out.Nodes = append(out.Nodes, subagent.NodeSnapshot{Node: subagent.Node{ID: rootID, SessionID: w.rootSessionID, State: subagent.NodeIdle}})
			if out.Root == "" {
				out.Root = rootID
			}
		}
	}
	for _, childID := range w.order {
		created := w.created[childID]
		if _, exists := findNode(out.Nodes, func(n subagent.Node) bool { return n.SessionID == childID || n.ID == created.NodeID }); exists {
			continue
		}
		parentID := created.ParentSessionID
		if parentID == "" {
			parentID = created.SessionID
		}
		node := subagent.Node{ID: created.NodeID, SessionID: childID, State: subagent.NodeStarting, CreatedAt: created.CreatedAt, UpdatedAt: created.CreatedAt}
		if s := w.sessions[childID]; s != nil {
			node.Agent = s.agent
		}
		insertChild(out.Nodes, parentID, node)
	}
	overlay(out.Nodes, w.sessions)
	if base == nil && len(out.Nodes) == 0 {
		return nil
	}
	return &out
}

// graftMissing adds observed nodes the canonical base does not know yet,
// keeping base nodes authoritative and topology parent-linked.
func graftMissing(base, observed []subagent.NodeSnapshot) []subagent.NodeSnapshot {
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, node := range nodes {
			if _, exists := findSnapshot(base, node.Node.ID); !exists {
				leaf := node
				leaf.Children = nil
				if node.Node.Parent == "" || !insertUnder(base, node.Node.Parent, leaf) {
					base = append(base, leaf)
				}
			}
			walk(node.Children)
		}
	}
	walk(observed)
	return base
}

func insertUnder(nodes []subagent.NodeSnapshot, parent subagent.NodeID, child subagent.NodeSnapshot) bool {
	for i := range nodes {
		if nodes[i].Node.ID == parent {
			nodes[i].Children = append(nodes[i].Children, child)
			return true
		}
		if insertUnder(nodes[i].Children, parent, child) {
			return true
		}
	}
	return false
}

func insertChild(nodes []subagent.NodeSnapshot, parentSessionID string, child subagent.Node) bool {
	for i := range nodes {
		if nodes[i].Node.SessionID == parentSessionID || nodes[i].Node.ID == subagent.SessionRootID(parentSessionID) {
			child.Parent = nodes[i].Node.ID
			nodes[i].Children = append(nodes[i].Children, subagent.NodeSnapshot{Node: child})
			return true
		}
		if insertChild(nodes[i].Children, parentSessionID, child) {
			return true
		}
	}
	return false
}

func overlay(nodes []subagent.NodeSnapshot, sessions map[string]*watchedSession) {
	for i := range nodes {
		node := &nodes[i].Node
		sessionID := node.SessionID
		if sessionID == "" {
			sessionID = strings.TrimPrefix(string(node.ID), "root:")
		}
		if s := sessions[sessionID]; s != nil {
			node.Cost = max(node.Cost, s.ownCost)
			node.BackgroundProcesses = s.backgroundProcesses
			if node.SessionID == "" {
				overlay(nodes[i].Children, sessions)
				continue
			}
			if node.Agent == "" {
				node.Agent = s.agent
			}
			switch {
			case node.State == subagent.NodeStopped || node.State == subagent.NodeFailed || node.State == subagent.NodeCompleted:
			case s.running:
				node.State = subagent.NodeRunning
			case s.known && (node.State == subagent.NodeRunning || node.State == subagent.NodeStarting):
				node.State = subagent.NodeIdle
			}
			if node.State == subagent.NodeStopped {
				node.NeedsAttention, node.WaitingOn, node.AttentionID = false, "", ""
			} else if node.State == subagent.NodeFailed {
				node.NeedsAttention, node.WaitingOn = true, "failed"
			} else {
				node.WaitingOn = s.waitingOn()
				var ids []string
				for id := range s.interactions {
					ids = append(ids, "interaction:"+id)
				}
				node.AttentionID = subagent.AttentionIdentity(ids...)
				node.NeedsAttention = node.WaitingOn != ""
			}
		}
		overlay(nodes[i].Children, sessions)
	}
}

func cloneNodes(nodes []subagent.NodeSnapshot) []subagent.NodeSnapshot {
	if nodes == nil {
		return nil
	}
	out := slices.Clone(nodes)
	for i := range out {
		out[i].Children = cloneNodes(out[i].Children)
	}
	return out
}

// startSubagentTreeWatch observes the descendant tree when no in-process tree
// exists, so remote views share topology, run state and attention. A tree
// observation has no cursor: every (re)connect is a fresh baseline.
func (a *App) startSubagentTreeWatch(ctx context.Context, handle runtime.SessionHandle, rootSessionID string, epoch uint64) {
	if handle == nil || rootSessionID == "" || handle.Metadata().Historical {
		return
	}
	// In-process handles publish topology through their runtime's live tree, so
	// only descendant usage is observed here; it has no live-tree equivalent.
	if a.LiveRuntimeTree() || runtime.IsLocalSessionHandle(handle) {
		go a.watchDescendantUsage(ctx, handle, rootSessionID, epoch)
		return
	}
	watch := newTreeWatch(rootSessionID)
	a.treeMu.Lock()
	if previous := a.treeWatch; previous != nil && previous.rootSessionID == rootSessionID {
		previous.mu.Lock()
		watch.created, watch.order = previous.created, previous.order
		for id, s := range previous.sessions {
			for resolved := range s.resolved {
				watch.session(id).resolved[resolved] = true
			}
		}
		previous.mu.Unlock()
	}
	a.treeWatch = watch
	a.treeMu.Unlock()
	go func() {
		attempt, rebaselines := 0, 0
		for ctx.Err() == nil {
			var observation runtime.Observation
			var err error
			if observer, ok := handle.(interface {
				ObserveTreeMetadata(ctx context.Context) (runtime.Observation, error)
			}); ok && a.SelectedSessionHistory() {
				observation, err = observer.ObserveTreeMetadata(ctx)
			} else {
				observation, err = handle.Observe(ctx, runtime.ObserveOptions{Tree: true, OrderedTree: true})
			}
			if err == nil && observation.SessionsAdded == nil && observation.TreeUpdates == nil {
				// A single-session observer cannot represent descendants.
				if observation.Cancel != nil {
					observation.Cancel()
				}
				return
			}
			if err == nil {
				// The root's tree in a baseline may predate a descendant it
				// already observes; a bounded fresh baseline places it.
				progress, unplaced := a.consumeTreeObservation(ctx, watch, observation, epoch, rebaselines < maxTreeRebaselines)
				if observation.Cancel != nil {
					observation.Cancel()
				}
				if unplaced {
					rebaselines++
				} else if progress {
					attempt = 0
				}
			} else if !retryTreeObservation(err) {
				return
			}
			timer := time.NewTimer(min(time.Duration(1<<min(attempt, 8))*50*time.Millisecond, 5*time.Second))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			attempt++
		}
	}()
}

// refreshSubagentTreeView re-overlays observed descendants after a new
// canonical baseline replaced the session's (possibly stale) tree.
func (a *App) refreshSubagentTreeView(ctx context.Context, epoch uint64) {
	a.treeMu.Lock()
	watch := a.treeWatch
	a.treeMu.Unlock()
	if watch == nil || !watch.observedDescendants() {
		return
	}
	a.emitSubagentTreeView(ctx, epoch)
}

func (w *treeWatch) observedDescendants() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.order) > 0 {
		return true
	}
	for id := range w.sessions {
		if id != w.rootSessionID {
			return true
		}
	}
	return false
}

func retryTreeObservation(err error) bool {
	var sessionErr *runtime.SessionError
	if errors.As(err, &sessionErr) {
		switch sessionErr.Kind {
		case runtime.SessionErrorUnsupported, runtime.SessionErrorInvalid, runtime.SessionErrorNotFound, runtime.SessionErrorStopped, runtime.SessionErrorClosed:
			return false
		}
	}
	return !errors.Is(err, runtime.ErrUnsupported)
}

// Each seeding call (one baseline batch, or one live addition) is bounded by these.
// The time budget is per App rather than a package variable: observation goroutines
// can outlive the test that started them and must not share a mutable knob.
const defaultDescendantSeedTimeout = 10 * time.Second

var maxDescendantSeeds = 32

// descendantSeedLimit is the per-call lookup budget for this App.
func (a *App) descendantSeedLimit() time.Duration {
	if a.descendantSeedBudget > 0 {
		return a.descendantSeedBudget
	}
	return defaultDescendantSeedTimeout
}

// watchDescendantUsage forwards descendant usage for in-process roots, whose
// topology the live runtime tree already publishes. A closed observation is
// re-established at most maxTreeRebaselines times, without any timer.
func (a *App) watchDescendantUsage(ctx context.Context, handle runtime.SessionHandle, rootSessionID string, epoch uint64) {
	watch := &treeWatch{rootSessionID: rootSessionID, sessions: map[string]*watchedSession{}}
	for attempt := 0; ctx.Err() == nil; attempt++ {
		observation, err := handle.Observe(ctx, runtime.ObserveOptions{Tree: true, OrderedTree: true})
		if err != nil {
			if !retryTreeObservation(err) || attempt >= maxTreeRebaselines {
				return
			}
			continue
		}
		a.consumeDescendantUsage(ctx, watch, observation, epoch)
		if observation.Cancel != nil {
			observation.Cancel()
		}
		if attempt >= maxTreeRebaselines {
			return
		}
	}
}

// consumeDescendantUsage is the topology-free counterpart of consumeTreeObservation:
// it seeds added descendants and forwards usage and shared-plan changes.
func (a *App) consumeDescendantUsage(ctx context.Context, watch *treeWatch, observation runtime.Observation, epoch uint64) {
	a.seedDescendantUsages(ctx, watch, observation.Initial, epoch)
	for _, envelope := range observation.Replay {
		a.forwardDescendantUsage(ctx, watch, envelope, epoch)
	}
	added, events, errs := observation.SessionsAdded, observation.Events, observation.Errors
	updates := observation.TreeUpdates
	for added != nil || events != nil || updates != nil {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if update.Snapshot != nil {
				a.seedDescendantUsages(ctx, watch, []runtime.SessionSnapshot{*update.Snapshot}, epoch)
			}
			for _, envelope := range update.Replay {
				a.forwardDescendantUsage(ctx, watch, envelope, epoch)
			}
			if update.Event != nil {
				a.forwardDescendantUsage(ctx, watch, *update.Event, epoch)
			}
		case snapshot, ok := <-added:
			if !ok {
				added = nil
				continue
			}
			a.seedDescendantUsages(ctx, watch, []runtime.SessionSnapshot{snapshot}, epoch)
		case envelope, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			a.forwardDescendantUsage(ctx, watch, envelope, epoch)
		case _, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			return
		}
	}
}

// seedDescendantUsages seeds descendant usage for one baseline or one live
// addition. Each call gets its own budget: at most maxDescendantSeeds lookups,
// and no new lookup once the descendant seed budget has elapsed within the call.
// Calls do not share a lifetime budget, so a late child is always seeded.
func (a *App) seedDescendantUsages(ctx context.Context, watch *treeWatch, snapshots []runtime.SessionSnapshot, epoch uint64) {
	started := time.Now()
	lookups := 0
	for _, snapshot := range snapshots {
		if lookups == maxDescendantSeeds || time.Since(started) >= a.descendantSeedLimit() {
			return
		}
		if a.seedDescendantUsage(ctx, watch, snapshot, epoch) {
			lookups++
		}
	}
}

// seedDescendantUsage publishes a descendant's current usage. It reports whether
// an agent-info lookup was attempted. Unchanged usage is skipped before lookup,
// and the epoch is checked after the lookup so an answer from a replaced view
// cannot publish stale usage.
func (a *App) seedDescendantUsage(ctx context.Context, watch *treeWatch, snapshot runtime.SessionSnapshot, epoch uint64) bool {
	sess := snapshot.Session
	if a.sessions == nil || epoch != a.bridgeEpoch.Load() || !descendantSeedable(sess, watch.rootSessionID) {
		return false
	}
	key := newSeedKey(sess)
	if watch.seedPublished(sess.ID, key) {
		return false
	}
	handle, err := a.sessions.SessionByID(sess.ID)
	if err != nil {
		return true
	}
	provider, ok := handle.(runtime.SessionAgentInfoProvider)
	if !ok {
		return false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, a.descendantSeedLimit())
	defer cancel()
	info, err := provider.SessionAgentInfo(lookupCtx)
	if err != nil || info.Agent == nil || epoch != a.bridgeEpoch.Load() {
		return true
	}
	if !a.deliverEvent(ctx, runtime.NewTokenUsageEvent(sess.ID, info.Agent.AgentName, runtime.SessionUsage(sess, info.Agent.ContextLimit))) {
		return true
	}
	watch.markSeeded(sess.ID, key)
	return true
}

// deliverEvent reports whether the event was handed to the bus. A dropped event
// is not recorded as published, so the next addition can seed it again.
func (a *App) deliverEvent(ctx context.Context, event any) bool {
	// A select with both cases ready picks either; a canceled delivery must not enqueue.
	if ctx.Err() != nil {
		return false
	}
	select {
	case a.events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

func newSeedKey(sess *session.Session) seedKey {
	input, output := sess.Usage()
	return seedKey{input: input, output: output, ownCost: sess.OwnCost()}
}

// seedPublished reports whether this usage was already published for the session.
func (w *treeWatch) seedPublished(id string, key seedKey) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.sessions[id]
	return s != nil && s.seeded && s.seedKey == key
}

func (w *treeWatch) markSeeded(id string, key seedKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.session(id)
	s.seeded, s.seedKey = true, key
}

// descendantSeedable reports whether a snapshot is a non-root session with
// recorded tokens, the only case where the restore seed carries usage.
func descendantSeedable(sess *session.Session, rootSessionID string) bool {
	if sess == nil || sess.ID == "" || sess.ID == rootSessionID {
		return false
	}
	input, output := sess.Usage()
	return input > 0 || output > 0
}

// forwardDescendantUsage republishes descendant usage and shared-plan changes.
// The root projection omits child usage; an idle root has no plan subscription.
func (a *App) forwardDescendantUsage(ctx context.Context, watch *treeWatch, envelope runtime.SessionEvent, epoch uint64) {
	a.forwardDescendantPlanChange(ctx, watch, envelope, epoch)
	usage, ok := envelope.Event.(*runtime.TokenUsageEvent)
	if !ok || usage.Usage == nil || envelope.Gap || envelope.SessionID == "" || envelope.SessionID == watch.rootSessionID {
		return
	}
	if usage.SessionID != envelope.SessionID {
		return
	}
	if epoch != a.bridgeEpoch.Load() {
		return
	}
	a.sendEvent(ctx, usage)
}

// An idle parent has no run-stream subscription to shared-plan mutations.
func (a *App) forwardDescendantPlanChange(ctx context.Context, watch *treeWatch, envelope runtime.SessionEvent, epoch uint64) {
	change, ok := envelope.Event.(*runtime.PlanChangedEvent)
	if !ok || envelope.Gap || envelope.SessionID == "" || envelope.SessionID == watch.rootSessionID || epoch != a.bridgeEpoch.Load() {
		return
	}
	a.sendEvent(ctx, change)
}

const maxTreeRebaselines = 8

// consumeTreeObservation projects one tree observation until it ends. With
// rebaseline set, it ends early (unplaced=true) when its baseline holds a
// descendant without a topology edge yet.
func (a *App) consumeTreeObservation(ctx context.Context, watch *treeWatch, observation runtime.Observation, epoch uint64, rebaseline bool) (progress, unplaced bool) {
	watch.reset(observation.Initial)
	a.seedDescendantUsages(ctx, watch, observation.Initial, epoch)
	if watch.observedDescendants() {
		a.emitSubagentTreeView(ctx, epoch)
	}
	apply := func(envelope runtime.SessionEvent) {
		a.forwardDescendantUsage(ctx, watch, envelope, epoch)
		if !envelope.Gap && watch.apply(envelope) {
			a.emitSubagentTreeView(ctx, epoch)
		}
	}
	for _, envelope := range observation.Replay {
		apply(envelope)
	}
	// Later descendants arrive with their spawn edge on the event stream.
	if rebaseline && a.treeHasUnplaced(watch) {
		return false, true
	}
	added, events, errs := observation.SessionsAdded, observation.Events, observation.Errors
	updates := observation.TreeUpdates
	for added != nil || events != nil || updates != nil {
		select {
		case <-ctx.Done():
			return progress, false
		case update, ok := <-updates:
			if !ok {
				return progress, false
			}
			progress = true
			if update.Snapshot != nil {
				watch.add(*update.Snapshot)
				a.seedDescendantUsages(ctx, watch, []runtime.SessionSnapshot{*update.Snapshot}, epoch)
				for _, seed := range update.Replay {
					apply(seed)
				}
				a.emitSubagentTreeView(ctx, epoch)
			} else if update.Event != nil {
				apply(*update.Event)
			}
		case snapshot, ok := <-added:
			if !ok {
				added = nil
				continue
			}
			progress = true
			watch.add(snapshot)
			a.seedDescendantUsages(ctx, watch, []runtime.SessionSnapshot{snapshot}, epoch)
			a.emitSubagentTreeView(ctx, epoch)
		case envelope, ok := <-events:
			if !ok {
				return progress, false
			}
			progress = true
			apply(envelope)
		case _, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			return progress, false
		}
	}
	return progress, false
}

// treeHasUnplaced reports an observed descendant missing from the merged tree.
func (a *App) treeHasUnplaced(watch *treeWatch) bool {
	sess := a.Session()
	if sess == nil || sess.ID != watch.rootSessionID {
		return false
	}
	merged := watch.merged(sess.GetSubagentTree())
	watch.mu.Lock()
	ids := make([]string, 0, len(watch.sessions))
	for id, s := range watch.sessions {
		if id != watch.rootSessionID && s.known {
			ids = append(ids, id)
		}
	}
	watch.mu.Unlock()
	for _, id := range ids {
		if merged == nil {
			return true
		}
		if _, ok := findNode(merged.Nodes, func(n subagent.Node) bool { return n.SessionID == id }); !ok {
			return true
		}
	}
	return false
}

func (a *App) emitSubagentTreeView(ctx context.Context, epoch uint64) {
	if epoch != a.bridgeEpoch.Load() {
		return
	}
	snapshot := a.SubagentTreeSnapshot()
	if snapshot == nil {
		return
	}
	select {
	case a.events <- runtime.SubagentTree(*snapshot):
	case <-ctx.Done():
	}
}
