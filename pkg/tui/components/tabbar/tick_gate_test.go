package tabbar

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSettledTickSkipsLayoutAndPreservesGeometry(t *testing.T) {
	ar := newMotionRuntime()
	tb := New(ar, 12)
	t.Cleanup(tb.StopAnimations)
	tabs := motionTabs(12, 0)
	tabs[0].Title = "界 é 👩‍💻"
	tabs[0].Activity = messages.TabActivityRunning
	tb.SetWidth(156)
	tb.SetTabs(tabs, 0)
	require.True(t, tb.unchangedTick())
	view, generation := tb.View(), tb.VisualGeneration()
	zones, bounds := append([]clickZone(nil), tb.zones...), append([]tabBound(nil), tb.dragBounds...)
	// No elapsed time changes: even offscreen tabs must not be restyled. The
	// allocator contract proves this skips computeLayouts/renderTab, not merely
	// their final equality comparison.
	require.Zero(t, testing.AllocsPerRun(100, func() { tb.Tick() }))
	require.Equal(t, view, tb.View())
	require.Equal(t, generation, tb.VisualGeneration())
	require.Equal(t, zones, tb.zones)
	require.Equal(t, bounds, tb.dragBounds)
	require.Equal(t, int32(1), ar.ActiveCount())
	advanceTabRuntime(t, ar, tb, ar.Now()+animation.Card.DefaultFrameDuration())
	require.NotEqual(t, view, tb.View())
	require.True(t, tb.TakeVisualDirty())
	require.Equal(t, zones, tb.zones)
	require.Equal(t, bounds, tb.dragBounds)
}

func TestTickGateRejectsUncommittedVisualAndLifecycleWork(t *testing.T) {
	ar := newMotionRuntime()
	tb := New(ar, 12)
	t.Cleanup(tb.StopAnimations)
	tabs := motionTabs(2, 0)
	tabs[0].IsRunning = true
	tb.SetWidth(100)
	tb.SetTabs(tabs, 0)
	require.True(t, tb.unchangedTick())
	tb.InvalidateCache()
	require.False(t, tb.unchangedTick())
	tb.View()
	tb.scrollPending = true
	require.False(t, tb.unchangedTick())
	tb.scrollPending = false
	tb.drag.pending = true
	require.False(t, tb.unchangedTick())
	tb.drag.pending = false
	tb.drag.active = true
	require.False(t, tb.unchangedTick())
	tb.drag.active = false
	tb.settlingDrop = &settlingDropState{}
	require.False(t, tb.unchangedTick())
	tb.settlingDrop = nil
	for _, transition := range []*animation.Transition{&tb.plusHoverAnim, &tb.plusAnim, &tb.scrollAnim, &tb.reorderAnim, &tb.settleAnim, &tb.dragAnim} {
		transition.Start(animation.ShortDuration, animation.Linear)
		require.False(t, tb.unchangedTick())
		transition.Cancel()
	}
	// Even a warmed render must be revisited when colors change independently
	// of an input message. The stale generation is restored by rendering.
	tb.themeGeneration = styles.ThemeGeneration() + 1
	require.False(t, tb.unchangedTick())
	tb.View()
	tb.agentColorGeneration = styles.AgentColorGeneration() + 1
	require.False(t, tb.unchangedTick())
	tb.View()
	require.True(t, tb.unchangedTick())
	tb.StopAnimations()
	tb.Tick()
	require.Zero(t, ar.ActiveCount())
	require.Nil(t, ar.Continue())
	tb.SetVisible(false)
	tb.Tick()
	require.Zero(t, ar.ActiveCount())
	tb.SetVisible(true)
	require.Equal(t, int32(1), ar.ActiveCount())
	// Metadata and width mutations still install clickable geometry immediately,
	// without depending on View or the next tick.
	tabs[0].Title = "replacement 界"
	tb.SetTabs(tabs, 0)
	tb.SetWidth(44)
	require.NotEmpty(t, tb.dragBounds)
	for _, bound := range tb.dragBounds {
		require.LessOrEqual(t, bound.end, 44)
	}
	tb.Update(tea.MouseMotionMsg{X: 43, Y: 0})
	require.NotNil(t, tb.zones)
}

func TestTickGateUsesLastRenderedGlyphAcrossWholeCycle(t *testing.T) {
	scheduler := &motionScheduler{now: time.Unix(1, 0)}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	tb := New(ar, 12)
	t.Cleanup(tb.StopAnimations)
	tabs := motionTabs(1, 0)
	tabs[0].IsRunning = true
	tb.SetTabs(tabs, 0)
	before := tb.View()
	cmd := ar.Continue()
	require.NotNil(t, cmd)
	cycle := time.Duration(len(animation.Card.Frames())) * animation.Card.DefaultFrameDuration()
	scheduler.now = scheduler.now.Add(cycle - animation.TickRate)
	_, accepted := ar.Accept(cmd().(animation.TickMsg))
	require.True(t, accepted)
	require.Equal(t, cycle, ar.Now())
	require.True(t, tb.unchangedTick(), "a whole cycle changes elapsed time but not the rendered glyph")
	require.Zero(t, testing.AllocsPerRun(100, func() { tb.Tick() }))
	require.Equal(t, before, tb.View())
	require.False(t, tb.TakeVisualDirty())
}

func TestTickGateIgnoresExternalClockWithoutVisibleIndicator(t *testing.T) {
	for _, offscreenBusy := range []bool{false, true} {
		t.Run(fmt.Sprint(offscreenBusy), func(t *testing.T) {
			ar := newMotionRuntime()
			external := ar.Subscribe()
			external.Start()
			t.Cleanup(external.Stop)
			tb := New(ar, 8)
			t.Cleanup(tb.StopAnimations)
			tabs := motionTabs(8, 0)
			tabs[7].IsRunning = offscreenBusy
			tb.SetWidth(30)
			tb.SetTabs(tabs, 0)
			require.False(t, tb.indicatorSub.IsActive())
			before := tb.View()
			for range 20 {
				cmd := ar.Continue()
				require.NotNil(t, cmd)
				_, ok := ar.Accept(cmd().(animation.TickMsg))
				require.True(t, ok)
				require.True(t, tb.unchangedTick(), "invisible glyph must not trigger layout on another owner's clock")
				require.Zero(t, testing.AllocsPerRun(1, func() { tb.Tick() }))
				require.Equal(t, before, tb.View())
				require.False(t, tb.TakeVisualDirty())
			}
			if offscreenBusy {
				tb.SetWidth(200)
				require.True(t, tb.indicatorSub.IsActive())
				glyph := busyGlyph(tb.View())
				require.Equal(t, animation.Card.FrameAt(ar.Now()), glyph)
				advanceTabRuntime(t, ar, tb, ar.Now()+animation.Card.DefaultFrameDuration())
				require.NotEqual(t, glyph, busyGlyph(tb.View()))
				tb.SetVisible(false)
				require.False(t, tb.indicatorSub.IsActive())
				tb.SetVisible(true)
				require.True(t, tb.indicatorSub.IsActive())
			}
		})
	}
}
