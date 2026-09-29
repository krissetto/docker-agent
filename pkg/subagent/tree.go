package subagent

import (
	"errors"
	"slices"
	"sync"
	"time"
)

var ErrNodeNotFound = errors.New("subagent node not found")

type nodeRecord struct {
	node Node
}

// Tree is a thread-safe registry of running subagent nodes. It is intentionally
// small: state changes update nodes and publish snapshots to subscribers.
type Tree struct {
	mu          sync.RWMutex
	nodes       map[NodeID]*nodeRecord
	children    map[NodeID][]NodeID
	subscribers map[chan Snapshot]struct{}
	now         func() time.Time
}

// Remove deletes a node from the registry. Callers remove descendants first.
func (t *Tree) Remove(id NodeID) error {
	t.mu.Lock()
	rec, ok := t.nodes[id]
	if !ok {
		t.mu.Unlock()
		return ErrNodeNotFound
	}
	if len(t.children[id]) != 0 {
		t.mu.Unlock()
		return errors.New("node still has children")
	}
	delete(t.nodes, id)
	delete(t.children, id)
	if rec.node.Parent != "" {
		children := t.children[rec.node.Parent]
		for i, child := range children {
			if child == id {
				t.children[rec.node.Parent] = append(children[:i], children[i+1:]...)
				break
			}
		}
	}
	snap := t.snapshotLocked()
	publishSnapshot(t.subscribersLocked(), snap)
	t.mu.Unlock()
	return nil
}

// NewTree returns an empty tree.
func NewTree() *Tree {
	return &Tree{
		nodes:       map[NodeID]*nodeRecord{},
		children:    map[NodeID][]NodeID{},
		subscribers: map[chan Snapshot]struct{}{},
		now:         time.Now,
	}
}

// AddSubtree atomically adds a batch of nodes and publishes one snapshot. The
// batch may contain roots or attach below existing nodes. Validation happens
// while the tree is locked, so errors leave existing nodes and observers
// untouched.
func (t *Tree) AddSubtree(nodes []Node) error {
	if len(nodes) == 0 {
		return nil
	}

	when := t.now()
	incoming := make(map[NodeID]Node, len(nodes))
	for _, n := range nodes {
		if n.ID == "" {
			return errors.New("node id is required")
		}
		if n.Agent == "" {
			return errors.New("agent name is required")
		}
		if _, exists := incoming[n.ID]; exists {
			return errors.New("node already exists")
		}
		if n.CreatedAt.IsZero() {
			n.CreatedAt = when
		}
		n.UpdatedAt = when
		if n.State == "" {
			n.State = NodeStarting
		}
		incoming[n.ID] = n
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range incoming {
		if _, exists := t.nodes[id]; exists {
			return errors.New("node already exists")
		}
	}
	for id, n := range incoming {
		seen := map[NodeID]struct{}{id: {}}
		for parent := n.Parent; parent != ""; {
			if _, exists := t.nodes[parent]; exists {
				break
			}
			parentNode, exists := incoming[parent]
			if !exists {
				return ErrNodeNotFound
			}
			if _, cycle := seen[parent]; cycle {
				return errors.New("tree contains a cycle")
			}
			seen[parent] = struct{}{}
			parent = parentNode.Parent
		}
	}

	for _, original := range nodes {
		n := incoming[original.ID]
		t.nodes[n.ID] = &nodeRecord{node: n}
		if n.Parent != "" {
			t.children[n.Parent] = append(t.children[n.Parent], n.ID)
		}
	}
	publishSnapshot(t.subscribersLocked(), t.snapshotLocked())
	return nil
}

// Add inserts a node.
func (t *Tree) Add(n Node) error {
	if n.ID == "" {
		return errors.New("node id is required")
	}
	if n.Agent == "" {
		return errors.New("agent name is required")
	}
	when := t.now()
	if n.CreatedAt.IsZero() {
		n.CreatedAt = when
	}
	n.UpdatedAt = when
	if n.State == "" {
		n.State = NodeStarting
	}

	t.mu.Lock()
	if _, exists := t.nodes[n.ID]; exists {
		t.mu.Unlock()
		return errors.New("node already exists")
	}
	if n.Parent != "" {
		if _, exists := t.nodes[n.Parent]; !exists {
			t.mu.Unlock()
			return ErrNodeNotFound
		}
	}
	t.nodes[n.ID] = &nodeRecord{node: n}
	if n.Parent != "" {
		t.children[n.Parent] = append(t.children[n.Parent], n.ID)
	}
	snap := t.snapshotLocked()
	publishSnapshot(t.subscribersLocked(), snap)
	t.mu.Unlock()

	return nil
}

// Update mutates a node and publishes a new snapshot.
func (t *Tree) Update(id NodeID, fn func(*Node)) error {
	t.mu.Lock()
	rec, ok := t.nodes[id]
	if !ok {
		t.mu.Unlock()
		return ErrNodeNotFound
	}
	fn(&rec.node)
	rec.node.UpdatedAt = t.now()
	snap := t.snapshotLocked()
	publishSnapshot(t.subscribersLocked(), snap)
	t.mu.Unlock()

	return nil
}

// Node returns a copy of the requested node.
func (t *Tree) Node(id NodeID) (Node, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	rec, ok := t.nodes[id]
	if !ok {
		return Node{}, false
	}
	return rec.node, true
}

// ChildCount reads the number of direct children without allocating a full
// topology snapshot. The boolean distinguishes an absent node from a leaf.
func (t *Tree) ChildCount(id NodeID) (int, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.nodes[id]; !ok {
		return 0, false
	}
	return len(t.children[id]), true
}

// NewNodeID mints a fresh short id that is not currently present in the tree,
// retrying on the rare collision.
func (t *Tree) NewNodeID() NodeID {
	t.mu.Lock()
	defer t.mu.Unlock()
	for {
		id := NewID()
		if _, exists := t.nodes[id]; !exists {
			return id
		}
	}
}

// Snapshot returns a stable, serialisable view of the tree.
func (t *Tree) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.snapshotLocked()
}

// Subscribe registers for live tree snapshots. The returned channel receives
// the current snapshot immediately and then future updates. The caller must
// call the returned cancel function.
func (t *Tree) Subscribe(buffer int) (<-chan Snapshot, func()) {
	if buffer < 1 {
		buffer = 1
	}
	ch := make(chan Snapshot, buffer)
	t.mu.Lock()
	t.subscribers[ch] = struct{}{}
	publishSnapshot([]chan Snapshot{ch}, t.snapshotLocked())
	t.mu.Unlock()
	return ch, func() { t.unregister(ch) }
}

func (t *Tree) unregister(ch chan Snapshot) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.subscribers[ch]; ok {
		delete(t.subscribers, ch)
		close(ch)
	}
}

func (t *Tree) snapshotLocked() Snapshot {
	roots := make([]NodeID, 0, len(t.nodes))
	for id, rec := range t.nodes {
		if rec.node.Parent == "" {
			roots = append(roots, id)
		}
	}
	slices.Sort(roots)
	s := Snapshot{Version: SnapshotVersion}
	for _, id := range roots {
		s.Nodes = append(s.Nodes, t.snapshotNodeLocked(id))
	}
	if len(roots) == 1 {
		s.Root = roots[0]
	}
	return s
}

func (t *Tree) snapshotNodeLocked(id NodeID) NodeSnapshot {
	rec := t.nodes[id]
	snap := NodeSnapshot{Node: rec.node}
	for _, childID := range t.children[id] {
		if _, ok := t.nodes[childID]; ok {
			snap.Children = append(snap.Children, t.snapshotNodeLocked(childID))
		}
	}
	return snap
}

func (t *Tree) subscribersLocked() []chan Snapshot {
	subs := make([]chan Snapshot, 0, len(t.subscribers))
	for ch := range t.subscribers {
		subs = append(subs, ch)
	}
	return subs
}

func publishSnapshot(subs []chan Snapshot, snap Snapshot) {
	for _, ch := range subs {
		select {
		case ch <- snap:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- snap:
			default:
			}
		}
	}
}
