package messages

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestParallelDelegationPendingSpinnersAndCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ar := animation.NewRuntime()
		m := NewScrollableView(ar, 120, 40, &service.SessionState{}).(*model)
		defer m.StopAnimations()
		calls := []tools.ToolCall{
			{ID: "spawn-a", Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: `{"agent":"alpha","task":"unfinished\`}},
			{ID: "spawn-b", Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: `{"agent":"beta","task":"unfinished`}},
			{ID: "send", Function: tools.FunctionCall{Name: subagent.ToolSendMessage, Arguments: `{"to":"parent","message":"unfinished`}},
		}
		for _, call := range calls {
			m.AddOrUpdateToolCall("root", call, tools.Tool{}, types.ToolStatusPending)
		}
		first := m.View()
		require.Contains(t, ansi.Strip(first), "Spawning alpha")
		require.Contains(t, ansi.Strip(first), "Spawning beta")
		require.Contains(t, ansi.Strip(first), "Messaging parent")
		m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "send", Function: tools.FunctionCall{Arguments: `\`}}, tools.Tool{}, types.ToolStatusPending)
		first = m.View()
		_, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		time.Sleep(200 * time.Millisecond) //nolint:forbidigo // Advances synctest's fake clock.
		tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		m.Update(tick)
		for i, view := range m.views {
			require.NotContains(t, view.View(), "⠋", "tool %s must advance", calls[i].ID)
		}
		require.NotEqual(t, first, m.View())
		m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "spawn-a", Function: tools.FunctionCall{Arguments: `{"agent":"alpha","task":"done"}`}}, tools.Tool{}, types.ToolStatusRunning)
		require.Equal(t, types.ToolStatusPending, m.messages[1].ToolStatus)
		m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "spawn-a", Response: `Spawned subagent "alpha" (child-a).`}, types.ToolStatusCompleted)
		require.True(t, ar.HasActive(), "other tool IDs still hold leases")
		m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "spawn-b", Response: "spawn failed"}, types.ToolStatusError)
		require.True(t, ar.HasActive(), "send remains pending")
		m.removePendingToolCallMessages()
		require.False(t, ar.HasActive(), "cancel releases final pending lease")
		rendered := ansi.Strip(m.View())
		require.Contains(t, rendered, "Spawned alpha")
		require.Contains(t, rendered, "spawn failed")
		require.NotContains(t, rendered, "Messaging")
	})
}
