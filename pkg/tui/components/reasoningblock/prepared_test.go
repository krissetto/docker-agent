package reasoningblock

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
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
