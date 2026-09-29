package subagentview

import (
	"slices"

	"github.com/docker/docker-agent/pkg/subagent"
)

// Root resolves the canonical view root, including persisted roots with stable IDs.
func Root(snapshot subagent.Snapshot, sessionID string, attached subagent.NodeID) (subagent.NodeSnapshot, bool) {
	if attached != "" {
		return Find(snapshot.Nodes, attached)
	}
	if sessionID != "" {
		if root, ok := Find(snapshot.Nodes, subagent.SessionRootID(sessionID)); ok {
			return root, true
		}
		var findSession func([]subagent.NodeSnapshot) (subagent.NodeSnapshot, bool)
		findSession = func(nodes []subagent.NodeSnapshot) (subagent.NodeSnapshot, bool) {
			for _, node := range nodes {
				if node.Node.SessionID == sessionID {
					return node, true
				}
				if found, ok := findSession(node.Children); ok {
					return found, true
				}
			}
			return subagent.NodeSnapshot{}, false
		}
		return findSession(snapshot.Nodes)
	}
	if root, ok := Find(snapshot.Nodes, snapshot.Root); ok {
		return root, true
	}
	if len(snapshot.Nodes) > 0 {
		return snapshot.Nodes[0], true
	}
	return subagent.NodeSnapshot{}, false
}

// Sorted copies the tree, ordering siblings newest first without changing topology.
func Sorted(nodes []subagent.NodeSnapshot) []subagent.NodeSnapshot {
	copy := slices.Clone(nodes)
	for i := range copy {
		copy[i].Children = Sorted(copy[i].Children)
	}
	slices.SortStableFunc(copy, func(a, b subagent.NodeSnapshot) int { return b.Node.CreatedAt.Compare(a.Node.CreatedAt) })
	return copy
}

func Active(state subagent.NodeState) bool { return state == subagent.NodeRunning }

func Counts(nodes []subagent.NodeSnapshot) (total, active, attention int) {
	for _, node := range nodes {
		total++
		if Active(node.Node.State) {
			active++
		}
		if node.Node.NeedsAttention {
			attention++
		}
		t, a, n := Counts(node.Children)
		total += t
		active += a
		attention += n
	}
	return
}

type Row struct {
	Node   subagent.Node
	Depth  int
	Parent subagent.NodeID
	Branch bool
	Guides string
}

func Rows(nodes []subagent.NodeSnapshot, collapsed map[subagent.NodeID]bool) []Row {
	var rows []Row
	var walk func([]subagent.NodeSnapshot, int, subagent.NodeID, string)
	walk = func(nodes []subagent.NodeSnapshot, depth int, parent subagent.NodeID, prefix string) {
		for i, node := range nodes {
			last := i == len(nodes)-1
			connector, next := "├─", "│ "
			if last {
				connector, next = "└─", "  "
			}
			rows = append(rows, Row{Node: node.Node, Depth: depth, Parent: parent, Branch: len(node.Children) > 0, Guides: prefix + connector})
			if !collapsed[node.Node.ID] {
				walk(node.Children, depth+1, node.Node.ID, prefix+next)
			}
		}
	}
	walk(nodes, 0, "", "")
	return rows
}
