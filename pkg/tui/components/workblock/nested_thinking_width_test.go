package workblock

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/components/tool/shell"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func expandedShellMessage(id string) *types.Message {
	call := tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf output"}`}}
	msg := types.ToolCallMessage("root", call, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = "alpha output\nbeta output"
	return msg
}

// shellHeaderRows returns the expanded shell header rows in visual order.
func shellHeaderRows(view string) []int {
	var rows []int
	for i, line := range strings.Split(ansi.Strip(view), "\n") {
		if strings.Contains(line, "✓ shell") {
			rows = append(rows, i)
		}
	}
	return rows
}

func TestExpandedThinkingShellMatchesWorkSideMarginsAndHitBounds(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	direct := shell.New(ar, expandedShellMessage("direct"), service.StaticSessionState{})
	direct.(interface{ SetExpanded(expanded bool) }).SetExpanded(true)

	thinking := reasoningblock.New(ar, "thinking", "root", service.StaticSessionState{})
	thinking.AddToolCall(expandedShellMessage("nested"))
	thinking.SetExpandedImmediately(true)
	thinking.SetToolCallExpandedImmediately("nested", true)

	model := New(ar)
	model.SetSize(60, 20)
	model.SetItems([]Item{{SourceIndex: 1, View: direct}, {SourceIndex: 2, View: thinking}})
	model.SetExpandedImmediately(true)
	t.Cleanup(model.StopAnimation)

	view := model.View()
	headers := shellHeaderRows(view)
	require.Len(t, headers, 2, "direct and nested shell headers are both visible: %q", ansi.Strip(view))
	plain := strings.Split(ansi.Strip(view), "\n")
	left := func(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }
	right := func(line string) int { return ansi.StringWidth(strings.TrimRight(line, " ")) }

	direct0, nested := plain[headers[0]], plain[headers[1]]
	require.Equal(t, left(direct0), left(nested), "nested shell starts at the same Work inset as a direct card")
	require.Equal(t, right(direct0), right(nested), "nested shell ends at the same Work inset as a direct card")
	require.Equal(t, 2, left(nested), "Work left margin is two columns")
	require.Equal(t, 58, right(nested), "Work right margin is two columns")

	nestedLine := headers[1]
	require.True(t, model.IsToggleAt(nestedLine, 2), "left edge of the nested card hits its header")
	require.True(t, model.IsToggleAt(nestedLine, 57), "right edge of the nested card hits its header")
	require.False(t, model.IsToggleAt(nestedLine, 58), "right Work margin is inert")
	require.False(t, model.IsToggleAt(nestedLine, 1), "left Work margin is inert")
}
