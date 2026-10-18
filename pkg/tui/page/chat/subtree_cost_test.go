package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

func TestChatSubtreeCostProviderSharesLiveSidebarTotal(t *testing.T) {
	t.Parallel()
	page := newLayoutTestPage(t, msgtypes.SidebarRight)
	page.sidebar.Update(runtime.SubagentTree(subagent.Snapshot{Root: "root:root", Nodes: []subagent.NodeSnapshot{{
		Node:     subagent.Node{ID: "root:root", SessionID: "root", Agent: "root", Cost: 1},
		Children: []subagent.NodeSnapshot{{Node: subagent.Node{ID: "child-node", SessionID: "child", Agent: "worker", Cost: 2, State: subagent.NodeCompleted}}},
	}}}))
	provider := SubtreeCostProvider(page)
	require.NotNil(t, provider)
	assert.Equal(t, 3.0, provider())
	own := 4.0
	page.sidebar.SetTokenUsage(&runtime.TokenUsageEvent{SessionID: "child", AgentContext: runtime.AgentContext{AgentName: "worker"}, Usage: &runtime.Usage{Cost: own, OwnCost: &own}})
	assert.Equal(t, 5.0, provider(), "an already-open dialog observes the same updated total")
}
