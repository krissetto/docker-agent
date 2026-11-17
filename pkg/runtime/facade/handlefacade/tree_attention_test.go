package handlefacade

import (
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestConvertTreePreservesAttentionOccurrence(t *testing.T) {
	id := subagent.AttentionIdentity("interaction:approval")
	snapshot := &subagent.Snapshot{Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{AttentionID: id}}}}
	converted := convertTree(snapshot)
	require.Equal(t, id, converted.Roots[0].AttentionID)
}
