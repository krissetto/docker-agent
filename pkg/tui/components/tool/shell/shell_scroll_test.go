package shell

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func newScrollableShell(t *testing.T, width int) *shellModel {
	t.Helper()
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "scroll", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf output")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = strings.Repeat("output row\n", 40)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(width, 0)
	view.SetExpanded(true)
	t.Cleanup(view.StopAnimation)
	return view
}

func TestShellCardConsumesWheelOnlyInsideScrollableBounds(t *testing.T) {
	view := newScrollableShell(t, 60)
	bodyRows := len(view.detailsLines())
	require.Positive(t, shellBodyMaxOffset(bodyRows))

	first, last := shellBodyOutputRows(bodyRows)
	require.True(t, view.WheelAt(first, 3, -1), "top output boundary is consumed")
	require.Zero(t, view.bodyOffset)
	require.True(t, view.WheelAt(last, 3, 1), "last output row is inside the consumed region")
	require.Equal(t, 1, view.bodyOffset)
	require.True(t, view.WheelAt(first, 3, 1_000_000))
	require.Equal(t, shellBodyMaxOffset(bodyRows), view.bodyOffset)
	require.True(t, view.WheelAt(last, 3, 1), "bottom output boundary is consumed")
	require.Equal(t, shellBodyMaxOffset(bodyRows), view.bodyOffset)

	require.False(t, view.WheelAt(0, 3, 1), "header is not part of the body")
	require.False(t, view.WheelAt(first-1, 3, 1), "padding above the output passes wheel through")
	require.False(t, view.WheelAt(last+1, 3, 1), "footer hint passes wheel through")
	require.False(t, view.WheelAt(1, -1, 1), "left of the card bubbles to the transcript")
	require.False(t, view.WheelAt(1, 60, 1), "right of the card bubbles to the transcript")
}

func TestShellCardBodyKeepsWidthAndRowBudgetAtEveryOffset(t *testing.T) {
	view := newScrollableShell(t, 60)
	bodyRows := len(view.detailsLines())
	for offset := 0; offset <= shellBodyMaxOffset(bodyRows); offset++ {
		view.bodyOffset = offset
		rows := strings.Split(view.details(), "\n")
		require.LessOrEqual(t, len(rows), maxBodyRows, "offset %d keeps the card within the row budget", offset)
		for _, row := range rows {
			require.Equal(t, 60, ansi.StringWidth(row), "offset %d keeps every card row at full width", offset)
		}
		require.Len(t, rows, shellBodyCardRows(bodyRows), "offset %d matches the consumed card height", offset)
	}
}

func TestShellScrollbarHintAndAnimationFollowWindowedScroll(t *testing.T) {
	view := newScrollableShell(t, 60)
	bodyRows := len(view.detailsLines())
	require.Contains(t, ansi.Strip(view.details()), "more lines · scroll")
	require.True(t, view.bodyScroll.NeedsScrollbar(), "windowed body reserves a scrollbar")

	first, _ := shellBodyOutputRows(bodyRows)
	require.False(t, view.WheelAt(1, 3, 1_000_000), "padding row is not a body wheel target")
	require.True(t, view.WheelAt(first, 3, 1_000_000))
	_ = view.details()
	require.Equal(t, shellBodyMaxOffset(bodyRows), view.bodyScroll.ScrollOffset())
	require.Contains(t, ansi.Strip(view.details()), "end of output")
	require.True(t, view.NeedsTick(), "scroll activity keeps the scrollbar reveal ticking")

	view.StopAnimation()
	require.False(t, view.bodyScroll.AnimationActive(), "stopping the card releases the scrollbar lease")
	require.False(t, view.NeedsTick())
}

func TestShellShortBodyHasNoScrollWindowOrHint(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "short", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf short")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = "short output"
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(60, 0)
	view.SetExpanded(true)
	t.Cleanup(view.StopAnimation)
	require.False(t, view.WheelAt(1, 3, 1), "a body that fits is never a wheel target")
	require.NotContains(t, ansi.Strip(view.details()), "scroll")
	require.LessOrEqual(t, strings.Count(view.details(), "\n")+1, maxBodyRows)
}
