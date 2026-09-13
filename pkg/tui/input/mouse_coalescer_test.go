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
	c.Filter(tea.MouseMotionMsg{X: 1, Y: 1})
	c.Filter(tea.MouseWheelMsg{X: 2, Y: 3, Button: tea.MouseWheelDown})
	c.Filter(tea.MouseWheelMsg{X: 4, Y: 5, Button: tea.MouseWheelDown})
	c.Filter(tea.MouseMotionMsg{X: 90, Y: 90})
	boundary := c.Filter(tea.MouseReleaseMsg{X: 4, Y: 5, Button: tea.MouseLeft}).(messages.PointerBoundaryMsg)
	require.True(t, boundary.Pending.HasWheel)
	require.Equal(t, 2, boundary.Pending.WheelDelta)
	require.Equal(t, 4, boundary.Pending.X)
	require.Equal(t, 5, boundary.Pending.Y)
	require.Nil(t, boundary.Pending.Motion)
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
