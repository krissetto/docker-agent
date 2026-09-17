package tabbar

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestWheelOverflowClampsAndLeavesRuntimeIdle(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tb.SetWidth(30)
	tabs := motionTabs(8, 0)
	tb.SetTabs(tabs, 0)
	before := ansi.Strip(tb.View())
	layouts := tb.computeLayouts()
	maxScroll := layouts[len(layouts)-1].endCol - (tb.width - plusButtonWidth - scrollArrowWidth)

	require.Nil(t, tb.Update(messages.WheelCoalescedMsg{Delta: 1}))
	assert.Equal(t, scrollStep, tb.scrollOffset)
	assert.NotEqual(t, before, ansi.Strip(tb.View()))
	require.Nil(t, tb.Update(messages.WheelCoalescedMsg{Delta: 1000}))
	assert.Equal(t, maxScroll, tb.scrollOffset)
	end := tb.View()
	require.Nil(t, tb.Update(messages.WheelCoalescedMsg{Delta: 1}))
	assert.Equal(t, maxScroll, tb.scrollOffset)
	assert.Equal(t, end, tb.View())
	require.Nil(t, tb.Update(messages.WheelCoalescedMsg{Delta: -1}))
	assert.Equal(t, maxScroll-scrollStep, tb.scrollOffset, "reversal moves immediately at the clamped edge")
	require.Nil(t, tb.Update(messages.WheelCoalescedMsg{Delta: -1000}))
	assert.Zero(t, tb.scrollOffset)
	assert.Equal(t, before, ansi.Strip(tb.View()))
	assert.Equal(t, tabs, tb.tabs)
	assert.Zero(t, tb.activeIdx)
	assert.Zero(t, runtime.ActiveCount())
	assert.Nil(t, runtime.Continue())
}

func TestWheelNonOverflowAndHiddenTabsDoNotScroll(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		tb := New(newMotionRuntime(), 8)
		tb.SetWidth(2*FixedTabWidth(8) + plusButtonWidth)
		tb.SetTabs(motionTabs(count, 0), 0)
		before := tb.View()
		for _, delta := range []int{1, 1000, -1000} {
			require.Nil(t, tb.Update(messages.WheelCoalescedMsg{Delta: delta}))
			assert.Zero(t, tb.scrollOffset)
			assert.Equal(t, before, tb.View())
			assert.Zero(t, tb.ar.ActiveCount())
		}
	}
}

func TestWheelCancelsPendingRevealWithoutSnappingToActive(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tb.SetWidth(30)
	tb.SetTabs(motionTabs(8, 0), 0)
	tb.SetTabs(motionTabs(8, 7), 7)
	require.True(t, tb.scrollPending)
	delay := ScrollDelayMsg{seq: tb.scrollSeq}

	tb.Update(messages.WheelCoalescedMsg{Delta: 1})
	assert.Equal(t, scrollStep, tb.scrollOffset)
	assert.False(t, tb.scrollPending)
	assert.False(t, tb.scrollAnim.Running())
	tb.Update(delay)
	assert.Equal(t, scrollStep, tb.scrollOffset, "cancelled reveal cannot override manual scrolling")
	tb.SetTabs(motionTabs(8, 7), 7)
	assert.Equal(t, scrollStep, tb.scrollOffset, "unchanged active tab must not undo manual scrolling")
	assert.Zero(t, runtime.ActiveCount())
	assert.Nil(t, runtime.Continue())
}

func TestWheelPreservesPendingAndActiveDragRelease(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "active"}[active], func(t *testing.T) {
			tb := New(newMotionRuntime(), 8)
			tb.SetWidth(42)
			tb.SetTabs(motionTabs(8, 0), 0)
			tb.View()
			x := tb.dragBounds[0].start + 2
			tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})
			if active {
				tb.Update(DragHoldMsg{seq: tb.drag.seq})
			}
			before := tb.drag
			tb.Update(messages.WheelCoalescedMsg{Delta: 1})
			assert.Equal(t, before, tb.drag, "wheel does not release capture or move the pointer")
			assert.Equal(t, scrollStep, tb.scrollOffset)
			tb.Update(tea.MouseMotionMsg{X: x + 3, Button: tea.MouseLeft})
			tb.Update(tea.MouseReleaseMsg{X: x + 3, Button: tea.MouseLeft})
			assert.False(t, tb.IsDragging())
			tb.StopAnimations()
			assert.Zero(t, tb.ar.ActiveCount())
		})
	}
}

func TestWheelStationaryDragReleaseDeliversSettleChain(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tb.SetWidth(42)
	tb.SetTabs(motionTabs(8, 0), 0)
	tb.View()
	x := tb.dragBounds[0].start + 2
	tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})
	tb.Update(DragHoldMsg{seq: tb.drag.seq})
	require.True(t, tb.drag.active)
	require.Zero(t, runtime.ActiveCount())
	tb.Update(messages.WheelCoalescedMsg{Delta: 1})
	cmd := tb.Update(tea.MouseReleaseMsg{X: x + 3, Button: tea.MouseLeft})
	require.True(t, tb.HasFloatingOverlay())
	drainWheelSettleCommands(t, tb, cmd)
}

func TestWheelSettlingHelpersDeliverInitialTick(t *testing.T) {
	for _, retarget := range []bool{false, true} {
		t.Run(map[bool]string{false: "begin", true: "retarget"}[retarget], func(t *testing.T) {
			tb := New(newMotionRuntime(), 8)
			tb.SetWidth(80)
			tb.SetTabs(motionTabs(3, 0), 0)
			var cmd tea.Cmd
			if retarget {
				tb.settlingDrop = &settlingDropState{sessionID: "b", currentX: 2, targetX: 2}
				cmd = tb.retargetSettlingDrop()
			} else {
				cmd = tb.beginSettlingDrop("b", 2, 20)
			}
			require.Nil(t, cmd, "component registers without scheduling")
			cmd = tb.ar.Continue()
			require.NotNil(t, cmd, "owner commits the first transition")
			drainWheelSettleCommands(t, tb, cmd)
		})
	}
}

func TestWheelTickRetargetDeliversInitialTick(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(80)
	tb.SetTabs(motionTabs(3, 0), 0)
	tb.settlingDrop = &settlingDropState{sessionID: "b", currentX: 2, targetX: 2}
	cmd := tb.Tick()
	require.Nil(t, cmd)
	cmd = tb.ar.Continue()
	require.NotNil(t, cmd)
	drainWheelSettleCommands(t, tb, cmd)
}

func drainWheelSettleCommands(t *testing.T, tb *TabBar, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd, tb.ar.Continue()}
	for steps := 0; len(queue) > 0; steps++ {
		require.Less(t, steps, 200, "settle commands must quiesce without rescuing their lease")
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		switch msg := next().(type) {
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case animation.TickMsg:
			if _, accepted := tb.ar.Accept(msg); accepted {
				queue = append(queue, tb.Tick(), tb.ar.Continue())
			}
		default:
			t.Fatalf("unexpected settle command message: %T", msg)
		}
	}
	assert.False(t, tb.HasFloatingOverlay())
	assert.False(t, tb.IsAnimating())
	assert.Zero(t, tb.ar.ActiveCount())
	assert.Nil(t, tb.ar.Continue())
}
