package reasoningblock

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestPreparedReasoningMatchesExpandedAndCollapsed(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, expanded := range []bool{false, true} {
		state := service.NewSessionState(nil)
		state.SetExpandThinking(expanded)
		block := New(animation.NewRuntime(), "block", "worker", state)
		block.SetSize(100, 0)
		block.AppendReasoning(strings.Repeat("## Heading\n\nSome **markdown** with a [link](https://example.com) and `code`.\n\n", 10000))
		expected := block.View()
		prepare := block.PrepareReasoning()
		require.NotNil(t, prepare)
		prepared := prepare()
		require.True(t, block.ApplyPreparedReasoning(prepared))
		start := time.Now()
		require.Equal(t, expected, block.View())
		t.Logf("expanded=%v prepared View=%s", expanded, time.Since(start))
		block.SetSize(80, 0)
		require.False(t, block.ApplyPreparedReasoning(prepared))
		block.SetSize(100, 0)
		require.True(t, block.ApplyPreparedReasoning(prepared))
		captured := block.PrepareReasoning()
		require.Nil(t, captured)
		theme := *original
		theme.Colors.Background = "#123456"
		styles.ApplyTheme(&theme)
		require.False(t, block.ApplyPreparedReasoning(prepared))
		styles.ApplyTheme(original)
		block.AppendReasoning("changed")
		require.False(t, block.ApplyPreparedReasoning(prepared))
	}
}

func TestPreparedReasoningRejectsSnapshotAfterToolCountChanges(t *testing.T) {
	newBlock := func(id string) *Model {
		block := New(animation.NewRuntime(), id, "worker", &service.SessionState{})
		block.SetSize(30000, 0)
		block.AppendReasoning(strings.Repeat("short reasoning ", 1100))
		require.Greater(t, len(block.Reasoning()), 16*1024)
		return block
	}
	addTool := func(block *Model) {
		block.AddToolCall(types.ToolCallMessage("worker", tools.ToolCall{
			ID:       "call-1",
			Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"/tmp/test.txt"}`},
		}, tools.Tool{Name: "read_file"}, types.ToolStatusRunning))
	}

	t.Run("rejects stale worker result", func(t *testing.T) {
		block := newBlock("prepared-stale-tool-count")
		t.Cleanup(block.StopAnimation)
		prepare := block.PrepareReasoning()
		require.NotNil(t, prepare)
		prepared := prepare()

		addTool(block)
		require.False(t, block.ApplyPreparedReasoning(prepared))
		require.Contains(t, ansi.Strip(block.View()), "Thinking [+] (1 tool)")
		require.True(t, block.IsToggleLine(0))
	})

	t.Run("refreshes applied cache after tool addition", func(t *testing.T) {
		block := newBlock("prepared-cache-tool-count")
		t.Cleanup(block.StopAnimation)
		prepare := block.PrepareReasoning()
		require.NotNil(t, prepare)
		prepared := prepare()
		require.True(t, block.ApplyPreparedReasoning(prepared))
		oldCache := block.cache

		addTool(block)
		require.Contains(t, ansi.Strip(block.View()), "Thinking [+] (1 tool)")
		require.NotSame(t, oldCache, block.cache)
		require.Equal(t, 1, block.cache.toolCount)
		require.True(t, block.IsToggleLine(0))
	})
}
