package tabbar

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type dragScheduler struct {
	now time.Time
}

func (s *dragScheduler) Now() time.Time { return s.now }
func (s *dragScheduler) Tick(delay time.Duration, deliver func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(delay)
		return deliver(s.now)
	}
}

func newDragTestBar() (*TabBar, *dragScheduler) {
	scheduler := &dragScheduler{now: time.Unix(1, 0)}
	bar := New(animation.NewRuntimeWithScheduler(scheduler), 0)
	bar.SetTabs([]messages.TabInfo{
		{SessionID: "a", Title: "First tab", IsActive: true},
		{SessionID: "b", Title: "第二 tab"},
		{SessionID: "c", Title: "Last"},
	}, 0)
	bar.View()
	return bar, scheduler
}

func motionTick(t *testing.T, bar *TabBar, cmd tea.Cmd) animation.TickMsg {
	t.Helper()
	require.NotNil(t, cmd)
	msg, ok := bar.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	assert.Nil(t, bar.Update(msg))
	return msg
}

func splitDrop(t *testing.T, cmd tea.Cmd) ([]messages.ReorderTabMsg, tea.Cmd) {
	t.Helper()
	var reorders []messages.ReorderTabMsg
	var tickCmd tea.Cmd
	for _, c := range cmd().(tea.BatchMsg) {
		switch msg := c().(type) {
		case messages.ReorderTabMsg:
			reorders = append(reorders, msg)
		case animation.TickMsg:
			require.Nil(t, tickCmd)
			// The timer command has been delivered; feed that same message to Accept.
			tickCmd = func() tea.Msg { return msg }
		default:
			t.Fatalf("unexpected release message %T", msg)
		}
	}
	return reorders, tickCmd
}

func TestDropUsesLiftedEdgesAndIncludesOffscreenTabs(t *testing.T) {
	bar, _ := newDragTestBar()
	bar.SetWidth(18)
	bar.View()
	require.Equal(t, 0, bar.dragLast)
	original := slices.Clone(bar.dragBounds)
	src, next := original[0], original[1]
	grab := 2
	bar.handleLeftClickDown(src.start + grab)
	boundary := (next.start+next.end)/2 - (src.end - src.start) + grab
	bar.handleMouseMotion(boundary)
	require.True(t, bar.drag.active)
	assert.True(t, bar.drag.isNoOp(), "an edge touching the midpoint does not cross it")
	cmd := bar.handleMouseMotion(boundary + 1)
	assert.Equal(t, 2, bar.drag.dropIdx)
	require.True(t, motionTick(t, bar, cmd).Dirty())
	bar.View()
	assert.Equal(t, original, bar.dragBounds)
	bar.handleMouseMotion(100)
	assert.Equal(t, 3, bar.drag.dropIdx, "targets can extend beyond the visible viewport")
}

func TestTabDragRelease(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to int
		mouseX   int
	}{
		{name: "left to right", from: 0, to: 2, mouseX: 100},
		{name: "right to left", from: 2, to: 0, mouseX: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bar, _ := newDragTestBar()
			x := bar.dragBounds[tc.from].start + 2
			assert.Nil(t, bar.Update(tea.MouseReleaseMsg{X: x, Button: tea.MouseLeft}))
			assert.Nil(t, bar.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft}))
			tick := bar.Update(tea.MouseMotionMsg{X: tc.mouseX, Button: tea.MouseLeft})
			require.True(t, bar.IsDragging())
			cmd := bar.Update(tea.MouseReleaseMsg{X: tc.mouseX, Button: tea.MouseLeft})
			require.NotNil(t, cmd)
			assert.Equal(t, messages.ReorderTabMsg{FromIdx: tc.from, ToIdx: tc.to}, cmd())
			assert.False(t, bar.IsDragging())
			assert.Nil(t, bar.Update(tea.MouseReleaseMsg{X: tc.mouseX, Button: tea.MouseLeft}))
			assert.EqualValues(t, 1, bar.ar.ActiveCount(), "settle reuses the existing tick registration")
			require.NotNil(t, tick)
		})
	}
}

func TestPendingDoesNotLiftAndActiveNoOpSettlesWithoutSwitch(t *testing.T) {
	bar, clock := newDragTestBar()
	x := bar.dragBounds[1].start + 2
	require.Nil(t, bar.handleLeftClickDown(x), "press schedules no hold timer")
	clock.now = clock.now.Add(time.Second)
	assert.Nil(t, bar.handleMouseMotion(x+dragThreshold-1))
	assert.True(t, bar.drag.pending)
	assert.Nil(t, bar.FloatingOverlay())
	cmd := bar.handleMouseRelease(x + 1)
	require.NotNil(t, cmd)
	assert.Equal(t, messages.SwitchTabMsg{SessionID: "b"}, cmd())

	bar.handleLeftClickDown(x)
	bar.handleMouseMotion(x + dragThreshold)
	require.True(t, bar.drag.active)
	assert.True(t, bar.drag.isNoOp())
	before := bar.FloatingOverlay()
	cmd = bar.handleMouseRelease(x + dragThreshold)
	require.NotNil(t, cmd, "a non-reordering drop still animates")
	require.Equal(t, before.X, bar.FloatingOverlay().X)
	assert.Equal(t, tabSettleDuration, bar.motion.transition.Duration())
	motionTick(t, bar, cmd)
	clock.now = clock.now.Add(tabSettleDuration)
	motionTick(t, bar, bar.ar.Continue())
	assert.Nil(t, bar.FloatingOverlay())
	assert.Zero(t, bar.ar.ActiveCount())

	bar.View()
	bar.handleLeftClickDown(x)
	bar.handleMouseMotion(100)
	bar.handleMouseMotion(x)
	assert.Nil(t, bar.handleMouseRelease(x), "returning to the press never synthesizes a click")
	assert.False(t, bar.IsDragging())
}

func TestTabDragCancel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel func(*TabBar)
	}{
		{"order changed", func(b *TabBar) { b.SetTabs([]messages.TabInfo{b.tabs[0], b.tabs[2], b.tabs[1]}, 0) }},
		{"count changed", func(b *TabBar) { b.SetTabs(b.tabs[:2], 0) }},
		{"dragged tab replaced", func(b *TabBar) {
			b.SetTabs([]messages.TabInfo{{SessionID: "other"}, b.tabs[1], b.tabs[2]}, 0)
		}},
		{"escape", func(b *TabBar) { b.Update(tea.KeyPressMsg{Code: tea.KeyEscape}) }},
		{"blur", func(b *TabBar) { b.Update(tea.BlurMsg{}) }},
		{"hidden width", func(b *TabBar) { b.SetWidth(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bar, _ := newDragTestBar()
			bar.handleLeftClickDown(2)
			tick := bar.handleMouseMotion(100)
			require.NotNil(t, tick)
			bar.SetTabs(slices.Clone(bar.tabs), 0)
			require.True(t, bar.IsDragging(), "same order should not cancel")
			before := bar.VisualGeneration()
			tc.cancel(bar)
			assert.False(t, bar.IsDragging())
			assert.Greater(t, bar.VisualGeneration(), before)
			assert.Zero(t, bar.ar.ActiveCount())
			assert.Nil(t, bar.handleMouseRelease(100))
			_, ok := bar.ar.Accept(tick().(animation.TickMsg))
			assert.False(t, ok, "cancelled transition must abandon its tick lease")
		})
	}
}

func TestTabDragSettleAndTickGating(t *testing.T) {
	bar, scheduler := newDragTestBar()
	idle := bar.View()
	width := lipgloss.Width(idle)
	generation := bar.VisualGeneration()
	idleSubscription := bar.ar.Subscribe()
	idleUpdate := idleSubscription.Start()
	assert.False(t, motionTick(t, bar, idleUpdate).Dirty())
	assert.Equal(t, generation, bar.VisualGeneration())
	idleSubscription.Stop()
	assert.Zero(t, bar.ar.ActiveCount())

	bar.handleLeftClickDown(2)
	next := bar.dragBounds[1]
	dragX := (next.start+next.end)/2 - bar.dragBounds[0].end + 5
	cmd := bar.handleMouseMotion(dragX)
	dragged := bar.View()
	assert.NotContains(t, dragged, "┃")
	assert.Equal(t, width, lipgloss.Width(dragged))
	assert.Equal(t, 1, lipgloss.Height(dragged))
	assert.NotEqual(t, idle, dragged)
	assert.Equal(t, dragX-2, bar.motion.floatX, "floating tab should retain its grab point")
	assert.Equal(t, messages.CloseTabMsg{SessionID: "a"}, bar.handleMiddleClick(dragX)(), "floating tab is the top click zone")
	assert.NotContains(t, ansi.Strip(dragged), "First tab", "the source is only drawn by the compositor")
	assert.Contains(t, ansi.Strip(bar.FloatingOverlay().Content), "First tab")
	assert.Equal(t, dragReflowDuration, bar.motion.transition.Duration())

	generation = bar.VisualGeneration()
	assert.True(t, motionTick(t, bar, cmd).Dirty())
	assert.Greater(t, bar.VisualGeneration(), generation)
	assert.NotEqual(t, dragged, bar.View())

	// Near the end, rounding has reached the final columns, but the
	// registered transition still has time left. That tick must stay clean.
	scheduler.now = scheduler.now.Add(dragReflowDuration - 3*animation.TickRate - time.Millisecond)
	motionTick(t, bar, bar.ar.Continue())
	generation = bar.VisualGeneration()
	assert.False(t, motionTick(t, bar, bar.ar.Continue()).Dirty())
	assert.Equal(t, generation, bar.VisualGeneration())
	require.EqualValues(t, 1, bar.ar.ActiveCount())

	// Finish the bystander reflow before release to exercise settle starting
	// a fresh tick registration and batching it with exactly one reorder.
	scheduler.now = scheduler.now.Add(tabSettleDuration)
	motionTick(t, bar, bar.ar.Continue())
	assert.Zero(t, bar.ar.ActiveCount())
	cmd = bar.handleMouseRelease(dragX)
	require.NotNil(t, cmd)
	reorders, tick := splitDrop(t, cmd)
	assert.Equal(t, []messages.ReorderTabMsg{{FromIdx: 0, ToIdx: 1}}, reorders)
	require.True(t, bar.motion.settling)
	assert.Nil(t, bar.handleMouseRelease(dragX))

	a, b, c := bar.tabs[0], bar.tabs[1], bar.tabs[2]
	bar.SetTabs([]messages.TabInfo{b, a, c}, 1)
	require.True(t, bar.motion.settling, "expected order acknowledges the drop without interrupting settle")
	assert.EqualValues(t, 1, bar.ar.ActiveCount())
	before := bar.FloatingOverlay().X
	assert.True(t, motionTick(t, bar, tick).Dirty())
	assert.NotEqual(t, before, bar.FloatingOverlay().X)
	scheduler.now = scheduler.now.Add(tabSettleDuration)
	assert.True(t, motionTick(t, bar, bar.ar.Continue()).Dirty())
	assert.False(t, bar.motion.settling)
	assert.Zero(t, bar.ar.ActiveCount())
	assert.Nil(t, bar.ar.Continue())
	finished := ansi.Strip(bar.View())
	assert.Less(t, strings.Index(finished, "第二 tab"), strings.Index(finished, "First tab"))

	idleUpdate = idleSubscription.Start()
	assert.False(t, motionTick(t, bar, idleUpdate).Dirty())
	idleSubscription.Stop()
	assert.Zero(t, bar.ar.ActiveCount())
}

func TestTabSettleCancelsForUnrelatedOrder(t *testing.T) {
	bar, _ := newDragTestBar()
	bar.handleLeftClickDown(2)
	bar.handleMouseMotion(100)
	bar.handleMouseRelease(100)
	require.True(t, bar.motion.settling)
	bar.SetTabs([]messages.TabInfo{bar.tabs[0], bar.tabs[2], bar.tabs[1]}, 0)
	assert.False(t, bar.motion.settling)
	assert.Zero(t, bar.ar.ActiveCount())
	assert.Nil(t, bar.handleMouseRelease(100))
}

func TestSettleRetargetsResizeAndTitleFromCurrentOverlay(t *testing.T) {
	bar, _ := newDragTestBar()
	bar.SetWidth(100)
	bar.View()
	bar.handleLeftClickDown(bar.dragBounds[0].start + 2)
	tick := bar.handleMouseMotion(61)
	bar.handleMouseRelease(61)
	a, b, c := bar.tabs[0], bar.tabs[1], bar.tabs[2]
	bar.SetTabs([]messages.TabInfo{b, c, a}, 2)
	motionTick(t, bar, tick)
	before := bar.FloatingOverlay().X
	bar.SetWidth(85)
	bar.SetMaxTitleLength(6)
	assert.Equal(t, before, bar.FloatingOverlay().X)
	assert.Equal(t, tabSettleDuration, bar.motion.transition.Duration())
	bar.View()
}

func TestExternalReorderAnimatesByStableIDWithoutOverlay(t *testing.T) {
	bar, clock := newDragTestBar()
	a, b, c := bar.tabs[0], bar.tabs[1], bar.tabs[2]
	initial := ansi.Strip(bar.View())
	tick := bar.SetTabs([]messages.TabInfo{b, a, c}, 1)
	require.Nil(t, bar.FloatingOverlay())
	assert.Equal(t, initial, ansi.Strip(bar.View()))
	assert.True(t, motionTick(t, bar, tick).Dirty())
	clock.now = clock.now.Add(tabSettleDuration)
	motionTick(t, bar, bar.ar.Continue())
	finished := ansi.Strip(bar.View())
	assert.Less(t, strings.Index(finished, "第二 tab"), strings.Index(finished, "First tab"))
	assert.Zero(t, bar.ar.ActiveCount())
}

func TestSettlingTabCanBeGrabbedAgainAtItsRenderedPosition(t *testing.T) {
	bar, _ := newDragTestBar()
	x := bar.dragBounds[1].start + 2
	bar.handleLeftClickDown(x)
	bar.handleMouseMotion(x + dragThreshold)
	bar.handleMouseRelease(x + dragThreshold)
	settling := bar.FloatingOverlay()
	require.NotNil(t, settling)
	grab := settling.X + 2
	bar.Update(tea.MouseClickMsg{X: grab, Button: tea.MouseLeft})
	require.True(t, bar.drag.pending)
	bar.Update(tea.MouseMotionMsg{X: grab + dragThreshold})
	require.True(t, bar.drag.active)
	assert.False(t, bar.motion.settling)
	assert.Equal(t, settling.X+dragThreshold, bar.FloatingOverlay().X)
}

func TestNonLeftPressCommitsTheLastDragPosition(t *testing.T) {
	bar, _ := newDragTestBar()
	bar.handleLeftClickDown(2)
	bar.handleMouseMotion(100)
	before := bar.FloatingOverlay().X
	cmd := bar.Update(tea.MouseClickMsg{X: 0, Button: tea.MouseRight})
	require.NotNil(t, cmd)
	assert.Equal(t, messages.ReorderTabMsg{FromIdx: 0, ToIdx: 2}, cmd())
	assert.False(t, bar.IsDragging())
	assert.Equal(t, before, bar.FloatingOverlay().X)
}
