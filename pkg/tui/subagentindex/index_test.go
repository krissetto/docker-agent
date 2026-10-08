package subagentindex

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestIndexesAreScopedAndResetIsAuthoritative(t *testing.T) {
	left, right := New(), New()
	left.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "same", Name: "left"}}, {Node: subagent.Node{ID: "gone", Name: "gone"}}}})
	right.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "same", Name: "right"}}}})

	leftName, _ := left.Name("same")
	rightName, _ := right.Name("same")
	assert.Equal(t, "left", leftName)
	assert.Equal(t, "right", rightName)

	left.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "same", Name: "new"}}}})
	_, exists := left.Name("gone")
	assert.False(t, exists)
	left.Clear()
	_, exists = left.Name("same")
	assert.False(t, exists)
}

func TestResolveMatchesCanonicalTraversal(t *testing.T) {
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: "first", SessionID: "collision", Name: "First", Agent: "worker"}, Children: []subagent.NodeSnapshot{
			{Node: subagent.Node{ID: "duplicate", SessionID: "shared", Agent: "earlier"}},
		}},
		{Node: subagent.Node{ID: "collision", SessionID: "parent", Agent: "director"}},
		{Node: subagent.Node{ID: "duplicate", SessionID: "shared", Name: "Last", Agent: "last"}},
		{Node: subagent.Node{ID: "", SessionID: "empty-node-session", Agent: "empty ID"}},
	}}
	index := New()
	assert.True(t, index.Reset(tree))
	for _, parent := range []string{"", "parent", "root-only", "collision", "shared"} {
		for _, sender := range []string{"", "first", "collision", "duplicate", "shared", "parent", "root-only", "missing", "empty-node-session"} {
			assert.Equal(t, lifecycle.ResolveInputReference(&tree, parent, sender, "fallback"), index.Resolve(parent, sender, "fallback"), "parent=%q sender=%q", parent, sender)
		}
	}
	var absent *Index
	assert.Equal(t, lifecycle.ResolveInputReference(nil, "parent", "parent", "fallback"), absent.Resolve("parent", "parent", "fallback"))
	index.Clear()
	assert.Equal(t, lifecycle.ResolveInputReference(nil, "", "duplicate", "fallback"), index.Resolve("", "duplicate", "fallback"))
}

func TestResetOwnsMetadataAndIgnoresMetricsAndActivity(t *testing.T) {
	tree := subagent.Snapshot{Root: "root", Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "first", SessionID: "session", Name: "First", Agent: "worker"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Agent: "helper"}}}}}}
	index := New()
	assert.True(t, index.Reset(tree))
	want := index.Resolve("", "session", "")
	tree.Nodes[0].Node.Name = "changed by caller"
	tree.Nodes[0].Children[0].Node.Agent = "changed child"
	assert.Equal(t, want, index.Resolve("", "session", ""), "index does not retain caller-owned nodes")
	name, _ := index.Name("child")
	assert.Equal(t, "helper", name)
	assert.True(t, index.Reset(tree))
	assert.Equal(t, "changed by caller", index.Resolve("", "session", "").Name)

	node := &tree.Nodes[0].Node
	node.State, node.NeedsAttention, node.WaitingOn, node.Error = subagent.NodeFailed, true, "tool", "failed"
	node.Cost, node.InputTokens, node.OutputTokens, node.ToolCalls = 2, 100, 50, 3
	node.CreatedAt, node.UpdatedAt = time.Unix(1, 0), time.Unix(2, 0)
	node.Description, node.Task = "description", "task"
	tree.Version, tree.Durability = 1, subagent.DurabilityDurable
	assert.False(t, index.Reset(tree))
	assert.Zero(t, testing.AllocsPerRun(100, func() { index.Reset(tree) }), "metrics/activity-only reset allocates nothing")
	assert.Zero(t, testing.AllocsPerRun(100, func() { indexReferenceSink = index.Resolve("", "session", "") }), "known references use precomputed scalar metadata")
}

func TestResetDetectsIdentityAndTopologyChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*subagent.Snapshot)
	}{
		{"id", func(s *subagent.Snapshot) { s.Nodes[0].Node.ID = "new" }},
		{"session", func(s *subagent.Snapshot) { s.Nodes[0].Node.SessionID = "new" }},
		{"name", func(s *subagent.Snapshot) { s.Nodes[0].Node.Name = "new" }},
		{"agent under alias", func(s *subagent.Snapshot) { s.Nodes[0].Node.Agent = "new" }},
		{"parent", func(s *subagent.Snapshot) { s.Nodes[0].Node.Parent = "new" }},
		{"root", func(s *subagent.Snapshot) { s.Root = "new" }},
		{"added", func(s *subagent.Snapshot) {
			s.Nodes = append(s.Nodes, subagent.NodeSnapshot{Node: subagent.Node{ID: "new"}})
		}},
		{"removed", func(s *subagent.Snapshot) { s.Nodes = s.Nodes[:1] }},
		{"order", func(s *subagent.Snapshot) { s.Nodes[0], s.Nodes[1] = s.Nodes[1], s.Nodes[0] }},
		{"nesting same DFS order", func(s *subagent.Snapshot) {
			s.Nodes[0].Children = []subagent.NodeSnapshot{s.Nodes[1]}
			s.Nodes = s.Nodes[:1]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{
				{Node: subagent.Node{ID: "duplicate", SessionID: "shared", Name: "First", Agent: "worker"}},
				{Node: subagent.Node{ID: "duplicate", SessionID: "shared", Name: "Last", Agent: "worker"}},
			}}
			index := New()
			assert.True(t, index.Reset(tree))
			assert.False(t, index.Reset(tree))
			tc.change(&tree)
			assert.True(t, index.Reset(tree))
			assert.False(t, index.Reset(tree))
			assert.Equal(t, lifecycle.ResolveInputReference(&tree, "", "shared", "fallback"), index.Resolve("", "shared", "fallback"))
		})
	}
	index := New()
	assert.False(t, index.Reset(subagent.Snapshot{}))
	assert.True(t, index.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "old"}}}}))
	assert.True(t, index.Reset(subagent.Snapshot{}))
	_, exists := index.Name("old")
	assert.False(t, exists)
}

func TestConcurrentIndexReadsAndResets(t *testing.T) {
	index := New()
	tree := subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "node", SessionID: "session", Agent: "worker"}}}}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				index.Resolve("", "session", "fallback")
				index.Name("node")
				index.Reset(tree)
				index.Clear()
			}
		})
	}
	wg.Wait()
}

var indexReferenceSink lifecycle.InputReference

func BenchmarkIndexReferences(b *testing.B) {
	for _, size := range []int{16, 128} {
		tree := subagent.Snapshot{Nodes: make([]subagent.NodeSnapshot, size)}
		for j := range tree.Nodes {
			tree.Nodes[j].Node = subagent.Node{ID: subagent.NodeID(fmt.Sprintf("node-%d", j)), SessionID: fmt.Sprintf("session-%d", j), Agent: "worker"}
		}
		index := New()
		index.Reset(tree)
		sender := fmt.Sprintf("session-%d", size-1)
		b.Run(fmt.Sprintf("indexed/nodes=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				indexReferenceSink = index.Resolve("", sender, "worker")
			}
		})
		b.Run(fmt.Sprintf("unchanged-reset/nodes=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				index.Reset(tree)
			}
		})
	}
}

func TestParentReferenceUsesExactAttachSessionAndCanonicalIdentity(t *testing.T) {
	index := New()
	assert.Equal(t, lifecycle.InputReference{Name: "parent"}, index.Parent())
	index.SetParent("parent-session-full", "root")
	fallback := lifecycle.InputReference{Kind: lifecycle.InputReferenceParent, ID: "parent-session-full", Name: "root", Agent: "root", DisplayID: subagent.ShortID("parent-session-full")}
	assert.Equal(t, fallback, index.Parent(), "subtree-only snapshots retain exact parent session routing")
	index.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "abcde-full-node", SessionID: "parent-session-full", Name: "Visible root", Agent: "root"}}}})
	assert.Equal(t, lifecycle.InputReference{Kind: lifecycle.InputReferenceParent, ID: "parent-session-full", Name: "Visible root", Agent: "root", DisplayID: "abcde"}, index.Parent())
	index.Reset(subagent.Snapshot{})
	assert.Equal(t, fallback, index.Parent(), "tree refresh cannot erase attach context")
	index.Clear()
	assert.Equal(t, lifecycle.InputReference{Name: "parent"}, index.Parent(), "clearing page identity cannot leak another session's parent")
}
