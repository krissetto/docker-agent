package lifecycle

import (
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
