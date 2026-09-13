package input

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestMouseCoalescerBoundaryPrecedesStaleTimer(t *testing.T) {
	c := NewMouseCoalescer()
	t.Cleanup(c.Stop)
	require.Nil(t, c.Filter(tea.MouseMotionMsg{X: 1, Y: 2}))
	seq := c.timerSequence
	require.Nil(t, c.Filter(tea.MouseMotionMsg{X: 8, Y: 9, Button: tea.MouseLeft}))
	click := tea.MouseClickMsg{X: 8, Y: 9, Button: tea.MouseLeft}
	boundary, ok := c.Filter(click).(messages.PointerBoundaryMsg)
	require.True(t, ok)
	require.Equal(t, click, boundary.Event)
	require.Equal(t, 8, boundary.Pending.Motion.X)
	require.Equal(t, tea.MouseLeft, boundary.Pending.Motion.Button)
	require.Nil(t, c.Filter(mouseFlushMsg{sequence: seq}), "late timer cannot replay motion after click")
	require.Nil(t, c.timer)
	release := tea.MouseReleaseMsg{X: 8, Y: 9, Button: tea.MouseLeft}
	require.Equal(t, release, c.Filter(release))
}

func TestMouseCoalescerWheelPositionAndReleaseBoundary(t *testing.T) {
	c := NewMouseCoalescer()
	t.Cleanup(c.Stop)
	motion := tea.MouseMotionMsg{X: 1, Y: 1, Button: tea.MouseLeft}
	require.Nil(t, c.Filter(motion))
	pending := c.Filter(tea.MouseWheelMsg{X: 2, Y: 3, Button: tea.MouseWheelDown}).(messages.PointerUpdateMsg)
	require.Equal(t, &motion, pending.Motion)
	pending = c.Filter(tea.MouseWheelMsg{X: 4, Y: 5, Button: tea.MouseWheelDown}).(messages.PointerUpdateMsg)
	require.Equal(t, messages.PointerUpdateMsg{X: 2, Y: 3, WheelDelta: 1, HasWheel: true}, pending)
	require.Nil(t, c.Filter(tea.MouseWheelMsg{X: 4, Y: 5, Button: tea.MouseWheelDown}))
	motion = tea.MouseMotionMsg{X: 90, Y: 90, Button: tea.MouseLeft}
	pending = c.Filter(motion).(messages.PointerUpdateMsg)
	require.Equal(t, messages.PointerUpdateMsg{X: 4, Y: 5, WheelDelta: 2, HasWheel: true}, pending)
	release := tea.MouseReleaseMsg{X: 90, Y: 90, Button: tea.MouseLeft}
	boundary := c.Filter(release).(messages.PointerBoundaryMsg)
	require.Equal(t, &motion, boundary.Pending.Motion)
	require.False(t, boundary.Pending.HasWheel)
	require.Equal(t, release, boundary.Event)
}

func TestMouseCoalescerFlushMarkerAndStop(t *testing.T) {
	c := NewMouseCoalescer()
	t.Cleanup(c.Stop)
	c.Filter(tea.MouseMotionMsg{X: 1, Y: 2})
	seq := c.timerSequence
	update := c.Filter(mouseFlushMsg{sequence: seq}).(messages.PointerUpdateMsg)
	require.Equal(t, 1, update.Motion.X)
	require.Nil(t, c.Filter(mouseFlushMsg{sequence: seq}))
	c.Filter(tea.MouseMotionMsg{X: 3, Y: 4})
	require.NotNil(t, c.timer)
	c.Filter(tea.BlurMsg{})
	require.Nil(t, c.timer)
	require.False(t, c.hasMotion)
	c.Filter(tea.MouseMotionMsg{X: 3, Y: 4})
	c.Stop()
	require.Nil(t, c.timer)
	require.False(t, c.timerPending)
	require.False(t, c.hasMotion)
	require.Nil(t, c.sender)
	c.Stop()
}

func TestMouseCoalescerInflightWakeCannotOvertakeBoundary(t *testing.T) {
	c := NewMouseCoalescer()
	t.Cleanup(c.Stop)
	captured := make(chan tea.Msg, 1)
	release := make(chan struct{})
	finished := make(chan struct{})
	c.SetSender(func(msg tea.Msg) { captured <- msg; <-release })
	c.Filter(tea.MouseMotionMsg{X: 11, Y: 12})
	// Stop the real timer; drive its callback at the adversarial handoff point.
	c.mu.Lock()
	c.timer.Stop()
	seq := c.timerSequence
	c.mu.Unlock()
	go func() { defer close(finished); c.flush(seq) }()
	wake := <-captured
	boundary := c.Filter(tea.MouseClickMsg{X: 11, Y: 12, Button: tea.MouseLeft}).(messages.PointerBoundaryMsg)
	require.Equal(t, 11, boundary.Pending.Motion.X)
	require.Nil(t, c.Filter(wake), "queued wake cannot deliver pre-click motion after click")
	c.Stop()
	close(release)
	<-finished
	require.Nil(t, c.timer)
	require.False(t, c.hasMotion)
}

func TestMouseCoalescerResizeFlushesOldGeometryAndStoppedWakeDrops(t *testing.T) {
	c := NewMouseCoalescer()
	defer c.Stop()
	c.Filter(tea.MouseMotionMsg{X: 19, Y: 5})
	seq := c.timerSequence
	resize := tea.WindowSizeMsg{Width: 30, Height: 10}
	boundary := c.Filter(resize).(messages.PointerBoundaryMsg)
	require.Equal(t, resize, boundary.Event)
	require.Equal(t, 19, boundary.Pending.Motion.X)
	require.Nil(t, c.Filter(mouseFlushMsg{sequence: seq}))
	c.Filter(tea.MouseMotionMsg{X: 8, Y: 5})
	seq = c.timerSequence
	c.Stop()
	require.Nil(t, c.Filter(mouseFlushMsg{sequence: seq}))
}

func TestMouseCoalescerWheelAxesAndCoordinatesStayOrdered(t *testing.T) {
	c := NewMouseCoalescer()
	t.Cleanup(c.Stop)
	for _, event := range []tea.MouseWheelMsg{
		{X: 5, Y: 2, Button: tea.MouseWheelRight},
		{X: 5, Y: 2, Button: tea.MouseWheelRight},
	} {
		require.Nil(t, c.Filter(event))
	}
	sequence := c.timerSequence
	pending := c.Filter(tea.MouseWheelMsg{X: 5, Y: 2, Button: tea.MouseWheelUp, Mod: tea.ModShift}).(messages.PointerUpdateMsg)
	require.Equal(t, messages.PointerUpdateMsg{X: 5, Y: 2, WheelDelta: 2, WheelHorizontal: true, HasWheel: true}, pending)
	require.Greater(t, c.timerSequence, sequence)
	require.Nil(t, c.Filter(mouseFlushMsg{sequence: sequence}), "old wake cannot flush the fresh axis")

	sequence = c.timerSequence
	pending = c.Filter(tea.MouseWheelMsg{X: 5, Y: 9, Button: tea.MouseWheelDown}).(messages.PointerUpdateMsg)
	require.Equal(t, messages.PointerUpdateMsg{X: 5, Y: 2, WheelDelta: -1, HasWheel: true}, pending)
	require.Greater(t, c.timerSequence, sequence)
	require.Nil(t, c.Filter(mouseFlushMsg{sequence: sequence}), "old wake cannot reroute the fresh position")

	pending = c.Filter(tea.MouseWheelMsg{X: 6, Y: 9, Button: tea.MouseWheelDown}).(messages.PointerUpdateMsg)
	require.Equal(t, messages.PointerUpdateMsg{X: 5, Y: 9, WheelDelta: 1, HasWheel: true}, pending)
	pending = c.Filter(tea.MouseWheelMsg{X: 6, Y: 9, Button: tea.MouseWheelLeft}).(messages.PointerUpdateMsg)
	require.Equal(t, messages.PointerUpdateMsg{X: 6, Y: 9, WheelDelta: 1, HasWheel: true}, pending)
	sequence = c.timerSequence
	release := tea.MouseReleaseMsg{X: 12, Y: 10, Button: tea.MouseLeft}
	boundary := c.Filter(release).(messages.PointerBoundaryMsg)
	require.Equal(t, messages.PointerUpdateMsg{X: 6, Y: 9, WheelDelta: -1, WheelHorizontal: true, HasWheel: true}, boundary.Pending)
	require.Equal(t, release, boundary.Event)
	require.Nil(t, c.Filter(mouseFlushMsg{sequence: sequence}))
	require.Equal(t, release, c.Filter(release), "the final wheel was consumed exactly once")
	require.Nil(t, c.timer)
}

func TestMouseCoalescerWheelCancellationKeepsAxisAndFlushesOnce(t *testing.T) {
	for _, buttons := range [][2]tea.MouseButton{
		{tea.MouseWheelLeft, tea.MouseWheelRight},
		{tea.MouseWheelUp, tea.MouseWheelDown},
	} {
		c := NewMouseCoalescer()
		t.Cleanup(c.Stop)
		require.Nil(t, c.Filter(tea.MouseWheelMsg{X: 3, Y: 4, Button: buttons[0]}))
		require.Nil(t, c.Filter(tea.MouseWheelMsg{X: 3, Y: 4, Button: buttons[1]}))
		sequence := c.timerSequence
		pending := c.Filter(mouseFlushMsg{sequence: sequence}).(messages.PointerUpdateMsg)
		require.Equal(t, messages.PointerUpdateMsg{X: 3, Y: 4, HasWheel: true, WheelHorizontal: buttons[0] == tea.MouseWheelLeft}, pending)
		require.Nil(t, c.Filter(mouseFlushMsg{sequence: sequence}))
		require.Nil(t, c.timer)
	}
}

func TestMouseCoalescerWheelToMotionFlushUsesFreshGeneration(t *testing.T) {
	c := NewMouseCoalescer()
	t.Cleanup(c.Stop)
	c.Filter(tea.MouseWheelMsg{X: 3, Y: 4, Button: tea.MouseWheelLeft})
	sequence := c.timerSequence
	motion := tea.MouseMotionMsg{X: 30, Y: 40, Button: tea.MouseLeft}
	pending := c.Filter(motion).(messages.PointerUpdateMsg)
	require.True(t, pending.WheelHorizontal)
	require.Equal(t, -1, pending.WheelDelta)
	require.Nil(t, c.Filter(mouseFlushMsg{sequence: sequence}))
	require.Greater(t, c.timerSequence, sequence)
	sequence = c.timerSequence
	pending = c.Filter(mouseFlushMsg{sequence: sequence}).(messages.PointerUpdateMsg)
	require.Equal(t, messages.PointerUpdateMsg{X: 30, Y: 40, Motion: &motion}, pending)
	require.Nil(t, c.Filter(mouseFlushMsg{sequence: sequence}))
	require.Nil(t, c.timer)
}
