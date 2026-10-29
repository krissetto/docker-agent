package message

import (
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Finalize drops streaming and render caches; output, selection and copy targets must match a fresh view.
func TestFinalizedAssistantKeepsOutputSelectionAndCopyTargets(t *testing.T) {
	content := "## Heading\n\nText with `code` and **bold**.\n\n- one\n- two\n\n```go\nfunc main() {}\n```\n\nTail paragraph.\n"
	for _, width := range []int{40, 100} {
		live := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", content), nil)
		want := live.Render(width)
		wantBlocks := append([]markdown.CodeBlock(nil), live.CodeBlocks()...)
		require.NotEmpty(t, wantBlocks)

		live.Finalize()
		require.False(t, live.HasLiveRenderState())
		require.Nil(t, live.streamLines)
		require.Equal(t, want, live.Render(width))
		require.Equal(t, wantBlocks, live.CodeBlocks())
		require.False(t, live.HasLiveRenderState(), "finalized renders must not re-arm caches")

		for _, selected := range []bool{true, false} {
			oracle := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", content), nil)
			oracle.SetSelected(selected)
			live.SetSelected(selected)
			require.Equal(t, oracle.Render(width), live.Render(width), "selected=%v width=%d", selected, width)
			require.Equal(t, oracle.CodeBlocks(), live.CodeBlocks(), "selected=%v width=%d", selected, width)
		}
	}
}

func TestFinalizedAssistantRearmsOnAppendWithOracleOutput(t *testing.T) {
	first := "## Stream\n\n- alpha\n- beta"
	more := "\n- gamma\n\n```go\nx := 1\n```\n"
	v := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", first), nil)
	v.Render(60)
	v.Finalize()
	v.AppendContent(more)

	oracle := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", first+more), nil)
	require.Equal(t, oracle.Render(60), v.Render(60))
	require.Equal(t, oracle.CodeBlocks(), v.CodeBlocks())
}

// User expansion is per-view state; finalize and resize must keep it and render exactly like a fresh expanded view.
func TestUserExpandedDisclosureSurvivesFinalizeAndResize(t *testing.T) {
	var lines []string
	for i := range 40 {
		lines = append(lines, "user line "+strconv.Itoa(i))
	}
	content := strings.Join(lines, "\n")
	live := New(animation.NewRuntime(), types.User(content), nil)
	live.SetSize(80, 30)
	collapsed := live.Render(80)
	require.Contains(t, ansi.Strip(collapsed), "[+] expand")

	live.Toggle()
	require.True(t, live.IsExpanded())
	live.Finalize()
	require.True(t, live.IsExpanded(), "finalize must not reset user disclosure")
	require.Contains(t, ansi.Strip(live.Render(80)), "[-] collapse")

	for _, width := range []int{50, 100} {
		live.SetSize(width, 30)
		oracle := New(animation.NewRuntime(), types.User(content), nil)
		oracle.SetSize(width, 30)
		oracle.Toggle()
		require.True(t, live.IsExpanded())
		require.Equal(t, oracle.Render(width), live.Render(width), "expanded width=%d", width)
	}

	live.Toggle()
	require.False(t, live.IsExpanded())
	live.SetSize(80, 30)
	require.Equal(t, collapsed, live.Render(80), "collapsing restores the original collapsed output")
}
