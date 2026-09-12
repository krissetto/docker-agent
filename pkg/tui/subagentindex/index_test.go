package subagentindex

import (
	"testing"

	"github.com/stretchr/testify/assert"

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
