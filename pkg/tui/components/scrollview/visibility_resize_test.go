package scrollview

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

// settleScrollbarReveal drives the finite reveal/fade lease to completion.
func settleScrollbarReveal(t *testing.T, runtime *animation.Runtime, view *Model) {
	t.Helper()
	for range 30 {
		if runtime.ActiveCount() == 0 {
			break
		}
		advanceVisibility(t, runtime, view)
	}
	require.Zero(t, runtime.ActiveCount())
}

func newBottomScrolledView(t *testing.T) (*animation.Runtime, *Model) {
	t.Helper()
	clock := &visibilityClock{now: time.Unix(1, 0)}
	runtime := animation.NewRuntimeWithScheduler(clock)
	view := New(WithReserveScrollbarSpace(true), WithAnimationRuntime(runtime))
	view.SetPosition(10, 20)
	view.SetSize(12, 4)
	view.SetContent([]string{"0", "1", "2", "3", "4", "5", "6", "7"}, 8)
	view.SetScrollOffset(4)
	require.EqualValues(t, 1, runtime.ActiveCount(), "real offset change reveals")
	settleScrollbarReveal(t, runtime, view)
	return runtime, view
}

func TestResizeClampAndReanchorDoNotRevealButScrollingDoes(t *testing.T) {
	runtime, view := newBottomScrolledView(t)

	view.SetSize(12, 6) // clamps the bottom offset 4 -> 2
	require.Equal(t, 2, view.ScrollOffset())
	require.Zero(t, runtime.ActiveCount(), "resize clamp must not reveal or schedule ticks")
	require.False(t, view.AnimationActive())
	require.NotContains(t, view.View(), "│")

	view.SetSize(12, 4)
	view.SetLayoutScrollOffset(4) // owner re-anchors the bottom after the viewport grows
	require.Equal(t, 4, view.ScrollOffset())
	require.Zero(t, runtime.ActiveCount(), "resize re-anchor must not reveal")
	require.NotContains(t, view.View(), "│")

	view.SetScrollOffset(3)
	require.EqualValues(t, 1, runtime.ActiveCount(), "real scrolling after the resize reveals")
}

func TestResizeClampDoesNotBlockHoverReveal(t *testing.T) {
	runtime, view := newBottomScrolledView(t)

	view.SetSize(12, 6)
	require.Zero(t, runtime.ActiveCount(), "resize clamp must not reveal")
	require.True(t, view.SetScrollbarPointer(view.ScrollbarX()-2, 21), "hover near the gutter still reveals")
	require.EqualValues(t, 1, runtime.ActiveCount())
}

// Input between SetSize and the next render is user scrolling, not re-anchoring.
func TestResizeThenWheelBeforeRenderStillReveals(t *testing.T) {
	runtime, view := newBottomScrolledView(t)

	view.SetSize(12, 6) // silent clamp 4 -> 2, no render yet
	require.Zero(t, runtime.ActiveCount())
	handled, _ := view.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	require.True(t, handled)
	require.Equal(t, 0, view.ScrollOffset())
	require.EqualValues(t, 1, runtime.ActiveCount(), "wheel before render reveals")
}

func TestResizeThenRenderThenProgrammaticScrollReveals(t *testing.T) {
	runtime, view := newBottomScrolledView(t)

	view.SetSize(12, 6)
	require.Zero(t, runtime.ActiveCount())
	view.View()
	view.SetScrollOffset(1)
	require.EqualValues(t, 1, runtime.ActiveCount(), "offset change after render reveals")
}

// Keyboard and programmatic scrolling before the first render must still reveal.
func TestProgrammaticScrollBeforeRenderReveals(t *testing.T) {
	runtime, view := newBottomScrolledView(t)

	view.SetSize(12, 6)
	require.Zero(t, runtime.ActiveCount())
	view.SetScrollOffset(0)
	require.EqualValues(t, 1, runtime.ActiveCount(), "programmatic scroll before render reveals")
}

func TestLayoutScrollOffsetMovesSilently(t *testing.T) {
	runtime, view := newBottomScrolledView(t)

	generation := view.VisualGeneration()
	view.SetLayoutScrollOffset(1)
	require.Equal(t, 1, view.ScrollOffset())
	require.True(t, view.Changed(generation), "layout move still repaints")
	require.Zero(t, runtime.ActiveCount(), "layout move must not reveal or schedule ticks")
}

// Keyboard scrolling before the first render must reveal through Update.
func TestKeyboardScrollBeforeRenderReveals(t *testing.T) {
	runtime, view := newBottomScrolledView(t)

	view.SetSize(12, 6)
	require.Zero(t, runtime.ActiveCount())
	handled, _ := view.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	require.True(t, handled)
	require.Zero(t, view.ScrollOffset())
	require.EqualValues(t, 1, runtime.ActiveCount(), "keyboard scroll before render reveals")
}
