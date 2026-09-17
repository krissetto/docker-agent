package tabbar

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestHiddenTabbarSettlesAndRejectsDelayedInput(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tb.SetWidth(40)
	tabs := motionTabs(4, 0)
	tabs[0].Activity = messages.TabActivityRunning
	tb.SetTabs(tabs, 0)
	tb.View()
	tb.Update(tea.MouseClickMsg{X: 2, Button: tea.MouseLeft})
	hold := DragHoldMsg{seq: tb.drag.seq}
	tb.armScrollPending()
	delay := ScrollDelayMsg{seq: tb.scrollSeq}
	tb.SetVisible(false)
	require.Zero(t, runtime.ActiveCount())
	assert.Empty(t, tb.View())
	assert.Zero(t, tb.Height())
	tb.Update(hold)
	tb.Update(delay)
	tb.Update(tea.MouseClickMsg{X: 2, Button: tea.MouseLeft})
	tb.SetWidth(18)
	tb.SetTabs(tabs, 2)
	tb.Tick()
	assert.False(t, tb.IsDragging())
	assert.False(t, tb.IsAnimating())
	assert.Zero(t, runtime.ActiveCount())
	assert.Nil(t, runtime.Continue())
	tb.SetVisible(true)
	tb.SetTabs(tabs, 0)
	tb.Update(ScrollDelayMsg{seq: tb.scrollSeq})
	advanceTabRuntime(t, runtime, tb, runtime.Now()+2*time.Second)
	assert.Equal(t, 1, tb.Height())
	assert.LessOrEqual(t, lipgloss.Width(tb.View()), 18)
	tb.SetTabs(tabs[:1], 99)
	assert.Positive(t, runtime.ActiveCount(), "single busy tab remains visible and animated")
	assert.NotEmpty(t, tb.View())
	tb.StopAnimations()
}

func TestStationaryDragStopsTickingAndResumesOnMotion(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tb.SetWidth(80)
	tb.SetTabs(motionTabs(4, 0), 0)
	tb.View()
	tb.Update(tea.MouseClickMsg{X: 2, Button: tea.MouseLeft})
	tb.Update(DragHoldMsg{seq: tb.drag.seq})
	require.True(t, tb.drag.active)
	assert.Zero(t, runtime.ActiveCount(), "stationary pointer does not animate")
	tb.Update(tea.MouseMotionMsg{X: 20})
	require.True(t, tb.dragAnim.Running())
	advanceTabRuntime(t, runtime, tb, runtime.Now()+time.Second)
	assert.Zero(t, runtime.ActiveCount())
	assert.Nil(t, runtime.Continue())
	_ = tb.TakeVisualDirty()
	tb.Tick()
	assert.False(t, tb.TakeVisualDirty())
	tb.Update(tea.MouseMotionMsg{X: 34})
	require.True(t, runtime.HasActive())
	tb.SetVisible(false)
	assert.Zero(t, runtime.ActiveCount())
}

func TestPointerOrderingChangesAndInvalidActiveIndicesAreSafe(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(80)
	tabs := motionTabs(3, 0)
	tabs[0].SessionID = "full-session-identity-not-a-prefix"
	tb.SetTabs(tabs, -12)
	tb.View()
	tb.Update(tea.MouseClickMsg{X: 2, Button: tea.MouseLeft})
	hold := DragHoldMsg{seq: tb.drag.seq}
	tb.SetTabs([]messages.TabInfo{tabs[1], tabs[2]}, 200)
	assert.False(t, tb.IsDragging())
	tb.Update(hold)
	assert.False(t, tb.IsDragging())
	assert.Empty(t, commandMessages(tb.Update(tea.MouseReleaseMsg{X: 2, Button: tea.MouseLeft})))
	msgs := commandMessages(tb.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl}))
	require.Len(t, msgs, 1)
	assert.Equal(t, tabs[2].SessionID, msgs[0].(messages.CloseTabMsg).SessionID)
	tb.SetTabs(tabs, 0)
	tabs[0].SessionID = "caller-mutated"
	msgs = commandMessages(tb.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl}))
	require.Len(t, msgs, 1)
	assert.Equal(t, "full-session-identity-not-a-prefix", msgs[0].(messages.CloseTabMsg).SessionID)
}

func TestTinyTabbarClipsHitsAndOffscreenBusyDoesNotTick(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tabs := motionTabs(8, 0)
	tabs[7].IsRunning = true
	tb.SetWidth(20)
	tb.SetTabs(tabs, 0)
	assert.Zero(t, runtime.ActiveCount(), "fully clipped activity has no visible frames")
	for width := 1; width <= 8; width++ {
		tb.SetWidth(width)
		assert.LessOrEqual(t, lipgloss.Width(tb.View()), width)
		for _, zone := range tb.zones {
			assert.GreaterOrEqual(t, zone.startX, 0)
			assert.LessOrEqual(t, zone.endX, width)
		}
	}
	tb.SetWidth(200)
	assert.Equal(t, int32(1), runtime.ActiveCount())
	tb.SetVisible(false)
	assert.Zero(t, runtime.ActiveCount())
}

func BenchmarkTabbarStableView(b *testing.B) {
	tb := New(newMotionRuntime(), 20)
	tb.SetWidth(100)
	tb.SetTabs(motionTabs(12, 0), 0)
	tb.View()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = tb.View()
	}
}

func BenchmarkTabbarFixedMotion(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		runtime := newMotionRuntime()
		tb := New(runtime, 20)
		tb.SetWidth(100)
		tb.SetTabs(motionTabs(12, 0), 0)
		tb.View()
		tb.Update(tea.MouseClickMsg{X: 2, Button: tea.MouseLeft})
		tb.Update(DragHoldMsg{seq: tb.drag.seq})
		tb.Update(tea.MouseMotionMsg{X: 38})
		for range 8 {
			cmd := runtime.Continue()
			if cmd == nil {
				break
			}
			runtime.Accept(cmd().(animation.TickMsg))
			tb.Tick()
		}
		tb.StopAnimations()
	}
}

func TestAnimatingTabbarLeavesEditorKeysUnhandled(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(40)
	tb.SetTabs(motionTabs(4, 0), 0)
	tb.scrollAnim.Start(scrollAnimDuration, animation.EaseOutQuint)
	assert.Nil(t, tb.Update(tea.KeyPressMsg{Text: "exact input"}))
	tb.SetCloseTabEnabled(false)
	assert.Nil(t, tb.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl}))
	tb.StopAnimations()
}

func TestRepeatedViewDoesNotAdvanceTabbarState(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tb.SetWidth(40)
	tb.SetTabs(motionTabs(4, 0), 0)
	tb.View()
	tb.Update(tea.MouseClickMsg{X: 2, Button: tea.MouseLeft})
	tb.Update(DragHoldMsg{seq: tb.drag.seq})
	tb.Update(tea.MouseMotionMsg{X: 20})
	tb.Update(tea.MouseReleaseMsg{X: 20, Button: tea.MouseLeft})
	require.True(t, tb.IsAnimating())

	scroll, generation := tb.scrollOffset, tb.VisualGeneration()
	overlayX, hadOverlay := tb.lastOverlayX, tb.hadOverlay
	clock, registrations := runtime.Now(), runtime.ActiveCount()
	first := tb.View()
	for range 4 {
		assert.Equal(t, first, tb.View())
		assert.Equal(t, scroll, tb.scrollOffset)
		assert.Equal(t, generation, tb.VisualGeneration())
		assert.Equal(t, overlayX, tb.lastOverlayX)
		assert.Equal(t, hadOverlay, tb.hadOverlay)
		assert.Equal(t, clock, runtime.Now())
		assert.Equal(t, registrations, runtime.ActiveCount())
	}

	_ = tb.TakeVisualDirty()
	advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.TickRate)
	assert.True(t, tb.TakeVisualDirty(), "render memoization cannot consume the next overlay frame")
	advanceTabRuntime(t, runtime, tb, runtime.Now()+time.Second)
	assert.Zero(t, runtime.ActiveCount())
	_ = tb.TakeVisualDirty()
	generation = tb.VisualGeneration()
	tb.View()
	tb.Tick()
	assert.Equal(t, generation, tb.VisualGeneration())
	assert.False(t, tb.TakeVisualDirty(), "settled output stays idle")
}
