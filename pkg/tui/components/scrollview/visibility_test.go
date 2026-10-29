package scrollview

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

type visibilityClock struct{ now time.Time }

func (c *visibilityClock) Now() time.Time { return c.now }
func (c *visibilityClock) Tick(_ time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		c.now = c.now.Add(50 * time.Millisecond)
		return create(c.now)
	}
}

func advanceVisibility(t *testing.T, runtime *animation.Runtime, view *Model) {
	t.Helper()
	cmd := runtime.Continue()
	require.NotNil(t, cmd)
	msg, ok := runtime.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	view.UpdateAnimation(msg)
}

func TestScrollbarVisibilityFollowsActualOffsetAndFadesAfterHold(t *testing.T) {
	clock := &visibilityClock{now: time.Unix(1, 0)}
	runtime := animation.NewRuntimeWithScheduler(clock)
	view := New(WithReserveScrollbarSpace(true), WithAnimationRuntime(runtime))
	view.SetSize(12, 4)
	view.SetContent([]string{"0", "1", "2", "3", "4", "5"}, 6)
	require.Zero(t, runtime.ActiveCount(), "content overflow alone does not reveal or animate")
	require.Contains(t, view.View(), "  ", "reserved geometry remains present while hidden")

	view.SetScrollOffset(1)
	require.EqualValues(t, 1, runtime.ActiveCount(), "real offset change registers a finite reveal")
	for range 30 { // 1s activity hold plus a 150ms fade-out.
		if runtime.ActiveCount() == 0 {
			break
		}
		advanceVisibility(t, runtime, view)
	}
	require.Zero(t, runtime.ActiveCount(), "fade settles without a perpetual idle tick")
	require.NotContains(t, view.View(), "│", "scrollbar is transparent again after the activity hold")
}

func TestScrollbarHoverHoldAndClear(t *testing.T) {
	clock := &visibilityClock{now: time.Unix(1, 0)}
	runtime := animation.NewRuntimeWithScheduler(clock)
	view := New(WithAnimationRuntime(runtime))
	view.SetSize(12, 4)
	view.SetPosition(10, 20)
	view.SetContent([]string{"0", "1", "2", "3", "4", "5"}, 6)

	require.True(t, view.SetScrollbarPointer(view.ScrollbarX()-2, 21), "near-gutter pointer starts the reveal")
	for range 3 {
		advanceVisibility(t, runtime, view)
	}
	require.Zero(t, runtime.ActiveCount(), "hover at full opacity has no tick lease")
	require.Contains(t, view.View(), "│")

	view.SetScrollbarPointer(0, 21)
	require.EqualValues(t, 1, runtime.ActiveCount(), "leaving before the hold expires keeps a finite timer")
	for range 30 { // 1s hold plus the 150ms fade.
		if runtime.ActiveCount() == 0 {
			break
		}
		advanceVisibility(t, runtime, view)
	}
	require.Zero(t, runtime.ActiveCount())
	require.NotContains(t, view.View(), "│")
}

func TestScrollbarVisibilityIgnoresBoundaryWheelGrowthAndHiddenActivity(t *testing.T) {
	clock := &visibilityClock{now: time.Unix(1, 0)}
	runtime := animation.NewRuntimeWithScheduler(clock)
	view := New(WithAnimationRuntime(runtime))
	view.SetSize(12, 4)
	view.SetContent([]string{"0", "1", "2", "3"}, 4)
	view.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	require.Zero(t, runtime.ActiveCount(), "failed wheel at boundary has no offset change")
	view.SetContent([]string{"0", "1", "2", "3", "4"}, 5)
	require.Zero(t, runtime.ActiveCount(), "content growth without viewport motion stays hidden")

	view.SetScrollbarActive(false)
	view.SetScrollOffset(1)
	require.Zero(t, runtime.ActiveCount(), "occluded content updates do not start work")
	require.False(t, view.SetScrollbarPointer(view.ScrollbarX(), 0))
	require.Zero(t, runtime.ActiveCount())

	view.SetScrollbarActive(true)
	view.SetScrollOffset(0)
	view.SetScrollOffset(1)
	require.EqualValues(t, 1, runtime.ActiveCount(), "resumed actual scrolling can reveal")
	view.StopAnimation()
	require.Zero(t, runtime.ActiveCount(), "stop releases the runtime lease")
}

func TestScrollbarDragDoesNotKeepAnimationLeaseWhenFullyVisible(t *testing.T) {
	clock := &visibilityClock{now: time.Unix(1, 0)}
	runtime := animation.NewRuntimeWithScheduler(clock)
	view := New(WithAnimationRuntime(runtime))
	view.SetSize(12, 4)
	view.SetContent([]string{"0", "1", "2", "3", "4", "5", "6", "7"}, 8)
	view.SetScrollbarPointer(view.ScrollbarX(), 1)
	view.UpdateMouse(tea.MouseClickMsg{X: view.ScrollbarX(), Y: 0, Button: tea.MouseLeft})
	require.True(t, view.IsDragging())
	for range 3 {
		advanceVisibility(t, runtime, view)
	}
	require.Zero(t, runtime.ActiveCount(), "a visible captured thumb needs no idle tick chain")
	view.SetScrollbarPointer(0, 1)
	require.True(t, view.IsDragging(), "hover departure cannot release capture")
	require.Contains(t, view.View(), "│", "captured thumb remains visible outside the gutter")
	view.UpdateMouse(tea.MouseReleaseMsg{X: 0, Y: 1, Button: tea.MouseLeft})
	require.False(t, view.IsDragging())
	require.EqualValues(t, 1, runtime.ActiveCount(), "release starts the finite activity hold")
	view.StopAnimation()
	require.Zero(t, runtime.ActiveCount())
}
