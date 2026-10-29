package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/grip"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// deterministicRootFixture builds the root on an immediate scheduler, so every
// tick completes synchronously and the clock is fully deterministic. The shared
// harness is unchanged; only the composer grip, built on the construction runtime,
// is rebound to this clock.
func deterministicRootFixture(tb testing.TB, width, height int) *appModel {
	tb.Helper()
	ar := animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
	root, _, _ := harnessRoot(tb, width, height, ar)
	root.composerGrip = grip.NewReveal(ar)
	return root
}

// settleRootTickCap bounds finite root leases: 120 ticks is about 2s at the real
// 16.67ms cadence, well above the 1000ms scrollbar hold plus its fade.
const settleRootTickCap = 120

// settleRootAnimations drives the root's ORIGINAL tick chain. The commands already
// returned by Update are consumed first; a fresh chain is armed only when none is
// pending (no Update has run). Each TickMsg reaches root.Update exactly once, which
// accepts it, and the commands that Update returns are driven in turn.
func settleRootAnimations(t *testing.T, root *appModel, returned ...tea.Cmd) {
	t.Helper()
	var pending []tea.Msg
	for _, cmd := range returned {
		pending = append(pending, collectMsgs(cmd)...)
	}
	if len(pending) == 0 && root.ar.ActiveCount() > 0 {
		pending = collectMsgs(root.ar.Continue())
	}
	for ticks := 0; root.ar.ActiveCount() > 0; ticks++ {
		require.Less(t, ticks, settleRootTickCap, "finite root animations must settle within the tick cap")
		require.NotEmpty(t, pending, "an active lease needs its owner's tick command")
		msg := pending[0]
		pending = pending[1:]
		_, cmd := root.Update(msg)
		pending = append(pending, collectMsgs(cmd)...)
	}
}

// composerHandleCenter is the centre cell of the compact handle on the separator row.
func composerHandleCenter(root *appModel) (int, int) {
	col := root.composerColumn()
	inner := col.W - appPaddingHorizontal
	length := min(grip.HorizontalLength, inner)
	left := (inner - length) / 2
	return col.X + styles.AppPadding + left + length/2, root.composerLayout().separatorTop
}

// The press-started grip reveal is nonvacuous, finite, and changes no height.
func TestComposerGripPressRevealIsFiniteAndHeightStable(t *testing.T) {
	root := deterministicRootFixture(t, 120, 40)
	settleRootAnimations(t, root)
	before := root.editorHeight
	x, y := composerHandleCenter(root)
	_, cmd := root.Update(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.True(t, root.isDragging)
	pending := collectMsgs(cmd)
	if len(pending) == 0 {
		pending = collectMsgs(root.ar.Continue())
	}
	require.Positive(t, root.ar.ActiveCount(), "the press starts a finite grip reveal")
	for ticks := 0; root.ar.ActiveCount() > 0; ticks++ {
		require.Less(t, ticks, settleRootTickCap, "the grip reveal must be finite")
		require.NotEmpty(t, pending, "the reveal needs its owner's tick command")
		msg := pending[0]
		pending = pending[1:]
		_, next := root.Update(msg)
		pending = append(pending, collectMsgs(next)...)
		require.Equal(t, before, root.editorHeight, "the grip reveal changes no height on every tick")
	}
	_, _ = root.Update(tea.MouseReleaseMsg{X: x, Y: -1, Button: tea.MouseLeft})
	require.False(t, root.isDragging)
}

// Actual handle hover repaints the grip; repeated identical hover is a cache hit;
// leaving the handle clears the hover and its repaint.
func TestComposerHandleHoverRepaintsGripAndClearsOnLeave(t *testing.T) {
	root := deterministicRootFixture(t, 120, 40)
	settleRootAnimations(t, root)
	baseline := root.View().Content
	x, y := composerHandleCenter(root)
	hover := tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseNone}
	_, hoverCmd := root.Update(hover)
	settleRootAnimations(t, root, hoverCmd)
	require.True(t, root.isHoveringHandle, "the handle centre is a direct handle hover")
	hovered := root.View().Content
	require.NotEqual(t, baseline, hovered, "handle hover repaints the grip")

	_, repeatCmd := root.Update(hover)
	settleRootAnimations(t, root, repeatCmd)
	require.True(t, root.viewCacheValid, "repeated identical hover is a cache hit")
	require.Equal(t, hovered, root.View().Content)

	_, leaveCmd := root.Update(tea.MouseMotionMsg{X: x, Y: 0, Button: tea.MouseNone})
	settleRootAnimations(t, root, leaveCmd)
	require.False(t, root.isHoveringHandle)
	require.Equal(t, baseline, root.View().Content, "leaving the handle clears the grip hover")
}
