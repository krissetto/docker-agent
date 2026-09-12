package subagentview

import "github.com/docker/docker-agent/pkg/subagent"

// Find depth-first searches nodes for id.
func Find(nodes []subagent.NodeSnapshot, id subagent.NodeID) (subagent.NodeSnapshot, bool) {
	for _, node := range nodes {
		if node.Node.ID == id {
			return node, true
		}
		if found, ok := Find(node.Children, id); ok {
			return found, true
		}
	}
	return subagent.NodeSnapshot{}, false
}
