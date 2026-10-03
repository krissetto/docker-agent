package lifecycle

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestInputIdentityResolverUsesCanonicalMappingNotIDShape(t *testing.T) {
	tree := &subagent.Snapshot{Nodes: []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: "abcde-full-canonical-node", SessionID: "session-child", Name: "Visible worker", Agent: "worker", State: subagent.NodeStopped}},
		{Node: subagent.Node{ID: "12345", SessionID: "parent-session", Agent: "director"}},
		{Node: subagent.Node{ID: "session-child", SessionID: "other-session", Agent: "collision"}},
	}}
	exact := ResolveInputReference(tree, "", "session-child", "stale")
	assert.Equal(t, "collision", exact.Name, "exact Node.ID wins over another node's SessionID")
	tree.Nodes = tree.Nodes[:2]
	bySession := ResolveInputReference(tree, "", "session-child", "stale")
	byNode := ResolveInputReference(tree, "", "abcde-full-canonical-node", "stale")
	assert.Equal(t, byNode, bySession)
	assert.Equal(t, InputReferenceNode, bySession.Kind)
	assert.Equal(t, "abcde-full-canonical-node", bySession.ID, "display truncation never mutates navigation")
	assert.Equal(t, "Visible worker (abcde)", bySession.Label())
	assert.Equal(t, "worker", bySession.Agent, "color keyed by canonical agent, not display alias")
	parent := ResolveInputReference(tree, "parent-session", "parent-session", "director")
	assert.Equal(t, InputReferenceParent, parent.Kind)
	assert.Equal(t, "parent-session", parent.ID)
	assert.Equal(t, "director (12345)", parent.Label())
	assert.Equal(t, parent, ResolveInputReference(tree, "parent-session", "12345", "director"))
	rootParent := ResolveInputReference(nil, "root-session-full", "root-session-full", "root")
	assert.Equal(t, InputReferenceParent, rootParent.Kind)
	assert.Equal(t, "root-session-full", rootParent.ID)
	assert.Equal(t, "root (root-)", rootParent.Label())
	unknown := ResolveInputReference(tree, "", "fffff", "unknown")
	assert.Equal(t, InputReferenceUnknown, unknown.Kind, "five hex characters are not proof of a node")
	assert.Empty(t, unknown.ID)
	assert.Equal(t, "unknown (fffff)", unknown.Label())
}

func TestInputIdentityResolverLastDepthFirstMatchAndEmptySender(t *testing.T) {
	tree := &subagent.Snapshot{Nodes: []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: "duplicate", SessionID: "shared", Agent: "first"}, Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "duplicate", SessionID: "parent", Agent: "last exact"}},
		}},
		{Node: subagent.Node{ID: "other", SessionID: "shared", Agent: "last session"}},
	}}
	assert.Equal(t, "last exact", ResolveInputReference(tree, "", "duplicate", "fallback").Name)
	assert.Equal(t, "last session", ResolveInputReference(tree, "", "shared", "fallback").Name)
	assert.Equal(t, InputReferenceParent, ResolveInputReference(tree, "parent", "duplicate", "fallback").Kind)
	assert.Equal(t, InputReference{Name: "fallback", Agent: "fallback"}, ResolveInputReference(tree, "", "", "fallback"))
}

var referenceSink InputReference

func referenceTree(size int) *subagent.Snapshot {
	tree := &subagent.Snapshot{Nodes: make([]subagent.NodeSnapshot, size)}
	for j := range tree.Nodes {
		tree.Nodes[j].Node = subagent.Node{ID: subagent.NodeID(fmt.Sprintf("node-%d", j)), SessionID: fmt.Sprintf("session-%d", j), Agent: "worker"}
	}
	return tree
}

func TestResolveInputReferenceAllocationsDoNotScaleWithTree(t *testing.T) {
	small, large := referenceTree(1), referenceTree(128)
	for _, sender := range []string{"missing", "session-0"} {
		measure := func(tree *subagent.Snapshot) float64 {
			return testing.AllocsPerRun(100, func() {
				referenceSink = ResolveInputReference(tree, "", sender, "fallback")
			})
		}
		assert.Equal(t, measure(small), measure(large), "sender=%s", sender)
		assert.LessOrEqual(t, measure(large), float64(2), "sender=%s", sender)
	}
}

// Preserve the prior copy/address traversal as a bounded allocation baseline.
func priorResolveInputReference(snapshot *subagent.Snapshot, parentSessionID, senderID, senderName string) InputReference {
	ref := InputReference{Name: senderName, Agent: senderName, DisplayID: subagent.ShortID(senderID)}
	if senderID == "" {
		return ref
	}
	var byID, bySession *subagent.Node
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, item := range nodes {
			node := item.Node
			if string(node.ID) == senderID {
				byID = &node
			}
			if node.SessionID == senderID {
				bySession = &node
			}
			walk(item.Children)
		}
	}
	if snapshot != nil {
		walk(snapshot.Nodes)
	}
	node := byID
	if node == nil {
		node = bySession
	}
	if node != nil {
		ref.Kind, ref.ID = InputReferenceNode, string(node.ID)
		ref.Name, ref.Agent, ref.DisplayID = node.DisplayName(), node.Agent, subagent.ShortID(string(node.ID))
	}
	if parentSessionID != "" && (senderID == parentSessionID || (node != nil && node.SessionID == parentSessionID)) {
		ref.Kind, ref.ID = InputReferenceParent, parentSessionID
	}
	return ref
}

func BenchmarkResolveInputReference(b *testing.B) {
	for _, size := range []int{16, 128} {
		tree := referenceTree(size)
		sender := fmt.Sprintf("session-%d", size-1)
		for _, resolver := range []struct {
			name    string
			resolve func(*subagent.Snapshot, string, string, string) InputReference
		}{{"prior-copy", priorResolveInputReference}, {"borrowed-traversal", ResolveInputReference}} {
			b.Run(fmt.Sprintf("%s/nodes=%d", resolver.name, size), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					referenceSink = resolver.resolve(tree, "", sender, "worker")
				}
			})
		}
	}
}
