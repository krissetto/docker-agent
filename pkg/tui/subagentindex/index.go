package subagentindex

import (
	"sync"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
)

// Index maps subagent node IDs to names for one chat-page/session view.
type Index struct {
	mu       sync.RWMutex
	names    map[subagent.NodeID]string
	snapshot *subagent.Snapshot
}

func New() *Index { return &Index{names: make(map[subagent.NodeID]string)} }

// Reset authoritatively rebuilds the index from snapshot.
func (i *Index) Reset(snapshot subagent.Snapshot) {
	names := make(map[subagent.NodeID]string)
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, node := range nodes {
			names[node.Node.ID] = node.Node.DisplayName()
			walk(node.Children)
		}
	}
	walk(snapshot.Nodes)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.names = names
	i.snapshot = &snapshot
}

func (i *Index) Clear() {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.names = make(map[subagent.NodeID]string)
	i.snapshot = nil
}

func (i *Index) Name(id subagent.NodeID) (string, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	name, ok := i.names[id]
	return name, ok
}

func (i *Index) Resolve(parentID, senderID, senderName string) lifecycle.InputReference {
	if i == nil {
		return lifecycle.ResolveInputReference(nil, parentID, senderID, senderName)
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return lifecycle.ResolveInputReference(i.snapshot, parentID, senderID, senderName)
}
