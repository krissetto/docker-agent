package subagentindex

import (
	"sync"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
)

// Index maps canonical node and session identities for one chat-page/session view.
// It owns scalar metadata only, never a caller's mutable snapshot.
type Index struct {
	mu                           sync.RWMutex
	parentSessionID, parentAgent string
	root                         subagent.NodeID
	identities                   []identity
	byID                         map[subagent.NodeID]int
	bySession                    map[string]int
}

type identity struct {
	id, parent  subagent.NodeID
	sessionID   string
	name, agent string
	children    int
	ref         lifecycle.InputReference
}

func New() *Index { return &Index{} }

// Reset authoritatively replaces identity metadata, returning whether it changed.
// Tree events also carry metrics/activity updates: compare only identity and
// topology in traversal order before allocating or rebuilding lookup maps.
func (i *Index) Reset(snapshot subagent.Snapshot) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	position := 0
	var same func([]subagent.NodeSnapshot) bool
	same = func(nodes []subagent.NodeSnapshot) bool {
		for j := range nodes {
			item := &nodes[j]
			node := &item.Node
			if position >= len(i.identities) {
				return false
			}
			old := &i.identities[position]
			if old.id != node.ID || old.parent != node.Parent || old.sessionID != node.SessionID || old.name != node.Name || old.agent != node.Agent || old.children != len(item.Children) {
				return false
			}
			position++
			if !same(item.Children) {
				return false
			}
		}
		return true
	}
	if i.root == snapshot.Root && same(snapshot.Nodes) && position == len(i.identities) {
		return false
	}

	i.root = snapshot.Root
	clear(i.identities)
	i.identities = i.identities[:0]
	i.byID = make(map[subagent.NodeID]int)
	i.bySession = make(map[string]int)
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for j := range nodes {
			item := &nodes[j]
			node := &item.Node
			position := len(i.identities)
			i.identities = append(i.identities, identity{
				id: node.ID, parent: node.Parent, sessionID: node.SessionID,
				name: node.Name, agent: node.Agent, children: len(item.Children),
				ref: lifecycle.InputReference{
					Kind: lifecycle.InputReferenceNode, ID: string(node.ID),
					Name: node.DisplayName(), Agent: node.Agent, DisplayID: subagent.ShortID(string(node.ID)),
				},
			})
			// Last depth-first match wins, just as in ResolveInputReference.
			i.byID[node.ID] = position
			i.bySession[node.SessionID] = position
			walk(item.Children)
		}
	}
	walk(snapshot.Nodes)
	return true
}

func (i *Index) Clear() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.root = ""
	i.parentSessionID, i.parentAgent = "", ""
	i.identities = nil
	i.byID = nil
	i.bySession = nil
}

func (i *Index) Name(id subagent.NodeID) (string, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	position, ok := i.byID[id]
	if !ok {
		return "", false
	}
	return i.identities[position].ref.Name, true
}

func (i *Index) Resolve(parentID, senderID, senderName string) lifecycle.InputReference {
	if i == nil || senderID == "" {
		return lifecycle.ResolveInputReference(nil, parentID, senderID, senderName)
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	position, ok := i.byID[subagent.NodeID(senderID)]
	if !ok {
		position, ok = i.bySession[senderID]
	}
	if !ok {
		return lifecycle.ResolveInputReference(nil, parentID, senderID, senderName)
	}
	entry := &i.identities[position]
	ref := entry.ref
	if parentID != "" && (senderID == parentID || entry.sessionID == parentID) {
		ref.Kind, ref.ID = lifecycle.InputReferenceParent, parentID
	}
	return ref
}

// SetParent records exact attach context, including when the parent is outside the local subtree.
func (i *Index) SetParent(sessionID, agent string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.parentSessionID, i.parentAgent = sessionID, agent
}

func (i *Index) Parent() lifecycle.InputReference {
	if i == nil {
		return lifecycle.InputReference{Name: "parent"}
	}
	i.mu.RLock()
	sessionID, agent := i.parentSessionID, i.parentAgent
	i.mu.RUnlock()
	if sessionID == "" {
		return lifecycle.InputReference{Name: "parent"}
	}
	if agent == "" {
		agent = "parent"
	}
	return i.Resolve(sessionID, sessionID, agent)
}
