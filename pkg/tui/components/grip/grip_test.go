package grip

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// manualScheduler delivers ticks synchronously with a controllable clock so
// reveal tests never sleep.
type manualScheduler struct{ now time.Time }

func (s *manualScheduler) Now() time.Time { return s.now }
func (s *manualScheduler) Tick(d time.Duration, f func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(d)
		return f(s.now)
	}
}

func newTestRuntime(t *testing.T) *animation.Runtime {
	t.Helper()
	ar := animation.NewRuntimeWithScheduler(&manualScheduler{now: time.Unix(0, 0)})
	t.Cleanup(ar.Stop)
	return ar
}

// tick runs one scheduled frame through the runtime and the reveal.
func tick(t *testing.T, ar *animation.Runtime, r *Reveal) (changed bool) {
	t.Helper()
	cmd := ar.Continue()
	require.NotNil(t, cmd, "an animating reveal must keep a tick scheduled")
	msg, ok := cmd().(animation.TickMsg)
	require.True(t, ok)
	accepted, ok := ar.Accept(msg)
	require.True(t, ok)
	return r.Tick(accepted)
}

func TestHorizontalGripIsHiddenAndRevealsCompactSegment(t *testing.T) {
	t.Parallel()
	for _, width := range []int{1, 3, 7, 8, 9, 20, 81, 200} {
		hidden := Horizontal(width, 0, false, 0)
		assert.Equal(t, strings.Repeat(" ", width), ansi.Strip(hidden))
		assert.Equal(t, width, lipgloss.Width(hidden))
		revealed := Horizontal(width, 0, false, 1)
		assert.Equal(t, min(HorizontalLength, width), strings.Count(ansi.Strip(revealed), "─"), "width %d", width)
		assert.Equal(t, width, lipgloss.Width(revealed))
		assert.True(t, HorizontalHandleAt(width, (width-min(HorizontalLength, width))/2))
		if width > HorizontalLength+2 {
			assert.False(t, HorizontalHandleAt(width, 0))
		}
	}
	assert.Empty(t, Horizontal(0, 0, false, 1))
	assert.Empty(t, Horizontal(-3, 0, false, 1))
}

func TestVerticalHiddenIsPlainBlankColumn(t *testing.T) {
	t.Parallel()
	lines := Vertical(12, 0, false, 0)
	require.Len(t, lines, 12)
	for _, line := range lines {
		assert.Equal(t, " ", line)
	}
	assert.Nil(t, Vertical(0, 0, false, 1))
}

func TestVerticalRevealsCenteredDots(t *testing.T) {
	t.Parallel()
	for _, height := range []int{1, 2, 3, 4, 5, 24, 25} {
		lines := Vertical(height, 0, false, 1)
		require.Len(t, lines, height)
		top, length := Rows(height)
		assert.Equal(t, min(3, height), length)
		assert.Equal(t, length, strings.Count(strings.Join(lines, ""), "│"), "height %d has a compact line segment", height)
		for i, line := range lines {
			assert.Equal(t, 1, ansi.StringWidth(line), "height %d row %d keeps one cell", height, i)
			if i >= top && i < top+length {
				assert.Equal(t, "│", ansi.Strip(line), "height %d row %d", height, i)
			} else {
				assert.Equal(t, " ", line, "height %d row %d", height, i)
			}
		}
		// The grip is centered: the blank margins differ by at most one row.
		assert.LessOrEqual(t, abs(top-(height-top-length)), 1, "height %d", height)
	}
}

func TestVerticalFadeAndActiveStyles(t *testing.T) {
	t.Parallel()
	full := Vertical(9, 0, false, 1)
	half := Vertical(9, 0, false, 0.5)
	hover := Vertical(9, 1, false, 1)
	active := Vertical(9, 0, true, 1)
	top, length := Rows(9)
	center := top + length/2
	base := styles.ResizeHandleStyle.Render("│")
	assert.Equal(t, base, full[center])
	assert.Equal(t, styles.HoverText(base, 1, styles.Resize), hover[center])
	assert.Equal(t, darkenedResizeStyle().Render("│"), active[center], "active drag darkens the base grip")
	baseR, baseG, baseB := styles.ColorToRGB(styles.Resize)
	activeR, activeG, activeB := styles.ColorToRGB(darkenedResizeStyle().GetForeground())
	assert.Less(t, activeR, baseR)
	assert.Less(t, activeG, baseG)
	assert.Less(t, activeB, baseB)
	assert.NotEqual(t, full[center], half[center], "partial visibility fades the dot")
	assert.Equal(t, "│", ansi.Strip(half[center]))
	assert.Equal(t, " ", half[0], "rows outside the grip stay plain")
}

func TestRevealHandleHighlightSharesReversibleLease(t *testing.T) {
	t.Parallel()
	ar := newTestRuntime(t)
	r := NewReveal(ar)
	r.SetHovered(true)
	r.SetHandleHovered(true)
	require.True(t, r.Animating())
	assert.EqualValues(t, 1, ar.ActiveCount(), "edge reveal and direct hover share one lease")
	assert.Zero(t, r.HandleHighlight())
	assert.Zero(t, r.Visibility())

	tick(t, ar, &r)
	assert.Equal(t, r.Visibility(), r.HandleHighlight(), "both states follow the same sidebar hover cadence")
	partial := r.HandleHighlight()
	require.Greater(t, partial, 0.0)
	require.Less(t, partial, 1.0)
	r.SetHandleHovered(false)
	require.True(t, r.Animating())
	tick(t, ar, &r)
	assert.Less(t, r.HandleHighlight(), partial, "direct hover reverses smoothly")
	for r.Animating() {
		tick(t, ar, &r)
	}
	assert.InDelta(t, 1, r.Visibility(), 0, "leaving the grip does not hide while still on its edge")
	assert.Zero(t, r.HandleHighlight())
	assert.Zero(t, ar.ActiveCount())
}

func TestRevealSnapsWithoutRuntime(t *testing.T) {
	t.Parallel()
	var r Reveal
	assert.Nil(t, r.SetHovered(true))
	assert.InDelta(t, 1, r.Visibility(), 0)
	assert.False(t, r.Animating())
	assert.Nil(t, r.SetHovered(false))
	assert.InDelta(t, 0, r.Visibility(), 0)
	r = NewReveal(nil)
	r.SetDragging(true)
	assert.InDelta(t, 1, r.Visibility(), 0)
}

func TestRevealHoverFadesInThenOutAndReleasesLease(t *testing.T) {
	t.Parallel()
	ar := newTestRuntime(t)
	r := NewReveal(ar)
	assert.Nil(t, ar.Continue(), "idle reveal schedules nothing")

	r.SetHovered(true)
	require.True(t, r.Animating())
	assert.EqualValues(t, 1, ar.ActiveCount())

	var frames int
	last := 0.0
	for r.Animating() {
		require.True(t, tick(t, ar, &r), "every frame of a running fade changes the value")
		assert.Greater(t, r.Visibility(), last)
		last = r.Visibility()
		frames++
		require.Less(t, frames, 20, "fade must finish in a bounded number of frames")
	}
	assert.InDelta(t, 1, r.Visibility(), 0)
	assert.GreaterOrEqual(t, frames, 5, "fade is animated, not a single jump")
	assert.EqualValues(t, 0, ar.ActiveCount(), "lease released once settled")
	assert.Nil(t, ar.Continue(), "no permanent ticks while hovering")

	assert.Nil(t, r.SetHovered(true), "repeated hover is a no-op")
	assert.False(t, r.Animating())

	r.SetHovered(false)
	require.True(t, r.Animating())
	for r.Animating() {
		tick(t, ar, &r)
	}
	assert.InDelta(t, 0, r.Visibility(), 0)
	assert.Nil(t, ar.Continue())
}

func TestRevealReverseMidFadeAndNoChangeNoDirty(t *testing.T) {
	t.Parallel()
	ar := newTestRuntime(t)
	r := NewReveal(ar)
	r.SetHovered(true)
	tick(t, ar, &r)
	tick(t, ar, &r)
	mid := r.Visibility()
	require.Greater(t, mid, 0.0)
	require.Less(t, mid, 1.0)

	r.SetHovered(false)
	require.True(t, r.Animating(), "reversing keeps the same lease")
	assert.EqualValues(t, 1, ar.ActiveCount(), "reversal never registers a second lease")
	tick(t, ar, &r)
	assert.Less(t, r.Visibility(), mid)
	for r.Animating() {
		tick(t, ar, &r)
	}
	assert.InDelta(t, 0, r.Visibility(), 0)

	// A settled reveal ignores stray ticks without claiming dirtiness.
	cmd := ar.Continue()
	assert.Nil(t, cmd)
	assert.False(t, r.Tick(animation.TickMsg{}))
}

func TestRevealEnterLeaveBeforeFirstTickReleasesLease(t *testing.T) {
	t.Parallel()
	ar := newTestRuntime(t)
	r := NewReveal(ar)
	r.SetHovered(true)
	r.SetHovered(false)
	assert.False(t, r.Animating())
	assert.EqualValues(t, 0, ar.ActiveCount())
	assert.InDelta(t, 0, r.Visibility(), 0)
}

func TestRevealDragPinsVisibleUntilRelease(t *testing.T) {
	t.Parallel()
	ar := newTestRuntime(t)
	r := NewReveal(ar)
	r.SetHovered(true)
	for r.Animating() {
		tick(t, ar, &r)
	}
	r.SetDragging(true)
	assert.True(t, r.Dragging())
	// The pointer drifts off the edge while dragging: stays fully visible.
	assert.Nil(t, r.SetHovered(false))
	assert.False(t, r.Animating())
	assert.InDelta(t, 1, r.Visibility(), 0)

	// Release off the edge: fades out.
	r.SetDragging(false)
	require.True(t, r.Animating())
	for r.Animating() {
		tick(t, ar, &r)
	}
	assert.InDelta(t, 0, r.Visibility(), 0)

	// Release while still on the edge: stays visible without any animation.
	r.SetHovered(true)
	r.SetDragging(true)
	for r.Animating() {
		tick(t, ar, &r)
	}
	assert.Nil(t, r.SetDragging(false))
	assert.False(t, r.Animating())
	assert.InDelta(t, 1, r.Visibility(), 0)
}

func TestRevealResetClearsEverything(t *testing.T) {
	t.Parallel()
	ar := newTestRuntime(t)
	r := NewReveal(ar)
	r.SetHovered(true)
	r.SetHandleHovered(true)
	r.SetDragging(true)
	tick(t, ar, &r)
	r.Reset()
	assert.False(t, r.Animating())
	assert.False(t, r.Dragging())
	assert.False(t, r.Hovered())
	assert.False(t, r.HandleHovered())
	assert.Zero(t, r.HandleHighlight())
	assert.InDelta(t, 0, r.Visibility(), 0)
	assert.EqualValues(t, 0, ar.ActiveCount())
	assert.Nil(t, ar.Continue())
}

func TestScrollbarDecorator(t *testing.T) {
	t.Parallel()
	assert.Nil(t, ScrollbarDecorator(0))
	assert.Nil(t, ScrollbarDecorator(-1))

	track := styles.TrackStyle.Render("│")
	thumb := styles.ThumbStyle.Render("│")
	view := strings.Join([]string{track, thumb, thumb, track}, "\n")

	dim := ScrollbarDecorator(1)
	require.NotNil(t, dim)
	out := dim(view)
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 4, "line count is preserved")
	for i, line := range lines {
		assert.Equal(t, 1, ansi.StringWidth(line), "row %d keeps one cell", i)
		assert.Equal(t, "│", ansi.Strip(line))
	}
	assert.NotEqual(t, view, out, "fully revealed grip dims the scrollbar")
	assert.NotEqual(t, lines[0], lines[1], "track and thumb remain distinguishable")

	partial := ScrollbarDecorator(0.5)(view)
	assert.NotEqual(t, out, partial, "dimming follows the reveal value")
	assert.Equal(t, "", dim(""))
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
