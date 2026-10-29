package shell

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type shellClock struct{ now time.Time }

func (c *shellClock) Now() time.Time { return c.now }
func (c *shellClock) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg { c.now = c.now.Add(delay); return create(c.now) }
}

func advanceShell(t *testing.T, ar *animation.Runtime, view *shellModel, d time.Duration) {
	t.Helper()
	for range int((d + animation.TickRate - 1) / animation.TickRate) {
		if ar.ActiveCount() == 0 {
			return
		}
		cmd := ar.Continue()
		require.NotNil(t, cmd)
		tick, ok := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok)
		_, _ = view.Update(tick)
	}
}

func TestShellScrollbarRevealHoldsAndFadesAfterScrollAndHover(t *testing.T) {
	clock := &shellClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "fade", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf fade")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = strings.Repeat("fade row\n", 40)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(60, 0)
	view.SetExpanded(true)
	t.Cleanup(view.StopAnimation)
	_ = view.View()
	require.False(t, view.NeedsTick(), "a settled windowed body with no scroll activity has no scrollbar lease")

	require.True(t, view.WheelAt(2, 2, 1))
	_ = view.View()
	require.True(t, view.NeedsTick(), "scroll activity reveals the scrollbar")
	advanceShell(t, ar, view, 2*animation.MediumDuration+animation.MediumDuration)
	require.False(t, view.NeedsTick(), "the scrollbar fades out after its hold expires")

	first, _ := shellBodyOutputRows(len(view.detailsLines()))
	gutter := shellCardBodyLeft() + agentmessage.InnerWidth(60) - 1
	require.False(t, view.SetHoveredAt(first, true), "line-only hover does not reveal the body scrollbar")
	require.False(t, view.SetHoveredPositionAt(first, shellCardBodyLeft(), true), "text column far from the gutter does not reveal")
	require.False(t, view.NeedsTick())
	require.True(t, view.SetHoveredPositionAt(first, gutter, true), "gutter column reveals the scrollbar")
	require.True(t, view.NeedsTick(), "hovering the gutter keeps the reveal ticking")
	require.False(t, view.SetHoveredPositionAt(first, gutter, true), "repeated gutter hover is idempotent")
	require.False(t, view.SetHoveredAt(first, true), "render reapplication keeps the gutter pointer")
	require.True(t, view.NeedsTick(), "line-only reapplication must not clear positional gutter hover")
	require.False(t, view.SetHoveredAt(-1, false), "leaving keeps the hold until it expires; the fade test covers the exit")
	advanceShell(t, ar, view, 2*animation.MediumDuration+animation.MediumDuration)
	require.False(t, view.NeedsTick())
}

func TestShellHeaderHoverDoesNotRevealBodyScrollbar(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "header", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf header")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = strings.Repeat("header row\n", 40)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(60, 0)
	view.SetExpanded(true)
	t.Cleanup(view.StopAnimation)
	_ = view.View()
	require.True(t, view.SetHoveredAt(0, true))
	require.False(t, view.NeedsTick(), "header hover emphasises the header without revealing the body scrollbar")
}

func TestShellCollapsedStopAnimationReleasesScrollbarLease(t *testing.T) {
	clock := &shellClock{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(clock)
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "collapse", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf collapse")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = strings.Repeat("collapse row\n", 40)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(60, 0)
	view.SetExpanded(true)
	_ = view.View()
	require.True(t, view.WheelAt(2, 2, 1))
	_ = view.View()
	require.True(t, view.NeedsTick())

	view.SetExpanded(false)
	view.StopAnimation()
	require.False(t, view.NeedsTick())
	require.Zero(t, ar.ActiveCount(), "stopping the card releases every animation lease")
	require.False(t, view.WheelAt(2, 2, 1), "collapsed card never consumes wheel")
}

func TestShellCollapsedCardNeverAllocatesBodyScrollview(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "lazy", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: shellArgs("printf lazy")}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	msg.Content = strings.Repeat("lazy row\n", 40)
	view := New(ar, msg, service.StaticSessionState{}).(*shellModel)
	view.SetSize(60, 0)
	_ = view.View()
	view.SetHoveredAt(0, true)
	require.Nil(t, view.bodyScroll, "a collapsed card does not allocate its body scrollview")
	view.SetExpanded(true)
	_ = view.View()
	require.NotNil(t, view.bodyScroll)
	view.StopAnimation()
}
