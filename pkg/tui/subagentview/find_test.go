package subagentview

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
)

func TestFindReturnsNestedNodeByValue(t *testing.T) {
	nodes := []subagent.NodeSnapshot{{Node: subagent.Node{ID: "root"}, Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child", Name: "worker"}}}}}
	found, ok := Find(nodes, "child")
	require.True(t, ok)
	assert.Equal(t, "worker", found.Node.Name)
	_, ok = Find(nodes, "missing")
	assert.False(t, ok)
}
