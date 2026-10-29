package scrollview

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A layout content clamp is silent; the next real scroll reveals.
func TestLayoutContentClampIsSilentAndNextScrollReveals(t *testing.T) {
	runtime, view := newBottomScrolledView(t)

	view.SetLayoutContent([]string{"0", "1", "2", "3", "4", "5"}, 6) // clamps the bottom offset 4 -> 2
	require.Equal(t, 2, view.ScrollOffset())
	require.Zero(t, runtime.ActiveCount(), "layout content clamp must not reveal or schedule ticks")
	require.False(t, view.AnimationActive())

	view.SetScrollOffset(1)
	require.EqualValues(t, 1, runtime.ActiveCount(), "real scrolling after the layout clamp reveals")
}

// A content shrink clamp through SetContent still reveals.
func TestContentShrinkClampStillRevealsThroughSetContent(t *testing.T) {
	runtime, view := newBottomScrolledView(t)

	view.SetContent([]string{"0", "1", "2", "3", "4", "5"}, 6) // clamps the bottom offset 4 -> 2
	require.Equal(t, 2, view.ScrollOffset())
	require.EqualValues(t, 1, runtime.ActiveCount(), "content shrink clamp through SetContent still reveals")
}
