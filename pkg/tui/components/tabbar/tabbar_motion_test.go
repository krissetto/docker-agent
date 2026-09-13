package tabbar

import (
	"fmt"
	"math/rand"
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

func motionTabs(n, active int) []messages.TabInfo {
	tabs := make([]messages.TabInfo, n)
	for i := range tabs {
		tabs[i] = messages.TabInfo{SessionID: string(rune('a' + i)), Title: "Session " + string(rune('A'+i)), IsActive: i == active}
	}
	return tabs
}

func boundForSession(t *testing.T, tb *TabBar, sessionID string) tabBound {
	t.Helper()
	for _, bound := range tb.dragBounds {
		if bound.sessionID == sessionID {
			return bound
		}
	}
	require.FailNow(t, "visible session bound missing", sessionID)
	return tabBound{}
}

func commandMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if _, ok := msg.(animation.TickMsg); ok {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, cmd := range batch {
			out = append(out, commandMessages(cmd)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func advanceTabRuntime(t *testing.T, runtime *animation.Runtime, tb *TabBar, target time.Duration) {
	t.Helper()
	cmd := runtime.EnsureRunning()
	for runtime.Now() < target {
		if cmd == nil {
			return
		}
		msg, ok := cmd().(animation.TickMsg)
		require.True(t, ok)
		_, ok = runtime.Accept(msg)
		require.True(t, ok)
		tb.Tick()
		cmd = runtime.Continue()
	}
}

func TestViewAndHeightKeepNewTabAvailable(t *testing.T) {
	for _, count := range []int{0, 1} {
		tb := New(newMotionRuntime(), 8)
		tb.SetWidth(80)
		tb.SetTabs(motionTabs(2, 0), 0)
		require.NotEmpty(t, tb.View())
		tb.SetTabs(motionTabs(count, 0), 0)
		assert.Equal(t, 1, tb.Height())
		assert.Contains(t, ansi.Strip(tb.View()), "+")
		assert.NotEmpty(t, tb.zones)
	}
}

func TestClickReleaseBeforeHoldSwitchesWithoutDrag(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(80)
	tb.SetTabs(motionTabs(3, 0), 0)
	tb.View()
	require.Len(t, tb.dragBounds, 3)
	x := tb.dragBounds[1].start + 2
	require.NotNil(t, tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft}))
	assert.True(t, tb.drag.pending)
	msgs := commandMessages(tb.Update(tea.MouseReleaseMsg{X: x, Button: tea.MouseLeft}))
	require.Len(t, msgs, 1)
	assert.Equal(t, "b", msgs[0].(messages.SwitchTabMsg).SessionID)
	assert.False(t, tb.IsDragging())
}

func TestHoldActivatesFloatingDragAndMidpointReflow(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(80)
	tabs := motionTabs(4, 0)
	tabs[1].Title = "DRAGME"
	tb.SetTabs(tabs, 0)
	tb.View()
	x := tb.dragBounds[1].start + 2
	tb.handleLeftClickDown(x)
	seq := tb.drag.seq
	tb.Update(DragHoldMsg{seq: seq})
	require.True(t, tb.drag.active)
	layer := tb.GetDragLayerInfo(100, 6)
	require.NotNil(t, layer)
	assert.Equal(t, 6, layer.Y)
	assert.Contains(t, layer.Content, "DRAGME")

	right := boundForSession(t, tb, "c")
	tb.handleMouseMotion((right.start+right.end)/2 + tb.drag.grabOffset + 1)
	assert.Greater(t, tb.drag.dropIdx, tb.drag.dragIdx)
	assert.NotZero(t, tb.dragOffsetTo["c"], "crossed bystander should reflow")
	assert.NotContains(t, tb.View(), "DRAGME", "drag source belongs only to floating layer")
}

func TestDropRequestsReorderAndSettlesAfterModelUpdate(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(100)
	tabs := motionTabs(4, 0)
	tb.SetTabs(tabs, 0)
	tb.View()
	b := tb.dragBounds[0]
	tb.drag = dragState{active: true, dragIdx: 0, dropIdx: 3, startX: b.start + 1, cursorX: b.end + 12, grabOffset: 1, seq: 1}
	msgs := commandMessages(tb.handleMouseRelease(b.end + 12))
	require.Len(t, msgs, 1)
	assert.Equal(t, messages.ReorderTabMsg{FromIdx: 0, ToIdx: 2}, msgs[0])

	reordered := []messages.TabInfo{tabs[1], tabs[2], tabs[0], tabs[3]}
	tb.SetTabs(reordered, 2)
	assert.True(t, tb.IsAnimating())
	assert.True(t, tb.HasFloatingOverlay())
	require.NotNil(t, tb.GetDragLayerInfo(120, 2))
}

func TestPressHoldMotionReleaseUsesStableIDsAndDeterministicTicks(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 10)
	tb.SetWidth(100)
	tabs := []messages.TabInfo{
		{SessionID: "session-alpha", Title: "Alpha", IsActive: true},
		{SessionID: "session-bravo", Title: "Bravo"},
		{SessionID: "session-charlie", Title: "Charlie"},
		{SessionID: "session-delta", Title: "Delta"},
	}
	tb.SetTabs(tabs, 0)
	tb.View()
	require.Len(t, tb.dragBounds, 4)

	source := tb.dragBounds[1]
	grabX := source.start + (source.end-source.start)/3
	holdCmd := tb.Update(tea.MouseClickMsg{X: grabX, Button: tea.MouseLeft})
	require.NotNil(t, holdCmd)
	require.True(t, tb.drag.pending)
	assert.False(t, tb.drag.active, "press remains visually pending below the hold threshold")
	assert.Nil(t, tb.GetDragLayerInfo(100, 4), "pending press must not flash a drag layer")

	// Advancing the component clock below the threshold does not activate drag;
	// only the matching delayed hold event may cross that state boundary.
	tb.Tick()
	assert.True(t, tb.drag.pending)
	tb.Update(DragHoldMsg{seq: tb.drag.seq})
	require.True(t, tb.drag.active)
	assert.False(t, tb.drag.pending)
	activeLayer := tb.GetDragLayerInfo(100, 4)
	require.NotNil(t, activeLayer)
	assert.Equal(t, grabX-tb.drag.grabOffset, activeLayer.X)
	assert.Contains(t, activeLayer.Content, "Bravo")
	assert.NotContains(t, tb.View(), "Bravo", "active source is rendered only in the floating layer")

	var third tabBound
	for _, bound := range tb.dragBounds {
		if bound.sessionID == "session-charlie" {
			third = bound
			break
		}
	}
	require.NotEmpty(t, third.sessionID)
	thirdMid := (third.start + third.end) / 2
	sourceWidth := source.end - source.start
	motionX := thirdMid + tb.drag.grabOffset - sourceWidth + 1
	tb.Update(tea.MouseMotionMsg{X: motionX})
	require.True(t, tb.dragAnim.Running())
	require.True(t, runtime.HasActive(), "bystander transition owns ticks without a perpetual drag subscription")
	require.Equal(t, 3, tb.drag.dropIdx)
	require.NotZero(t, tb.dragOffsetTo["session-charlie"])
	assert.Equal(t, motionX-tb.drag.grabOffset, tb.GetDragLayerInfo(100, 4).X)

	advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.TickRate)
	intermediate := tb.dragPreviewOffset("session-charlie")
	assert.NotZero(t, intermediate)
	assert.NotEqual(t, tb.dragOffsetTo["session-charlie"], intermediate,
		"elapsed tick must expose an intermediate bystander displacement")

	msgs := commandMessages(tb.Update(tea.MouseReleaseMsg{X: motionX, Button: tea.MouseLeft}))
	require.Len(t, msgs, 1)
	assert.Equal(t, messages.ReorderTabMsg{FromIdx: 1, ToIdx: 2}, msgs[0])
	assert.False(t, tb.IsDragging())

	reordered := []messages.TabInfo{tabs[0], tabs[2], tabs[1], tabs[3]}
	tb.SetTabs(reordered, 0)
	require.True(t, tb.HasFloatingOverlay())
	settlingStart := tb.GetDragLayerInfo(100, 4)
	require.NotNil(t, settlingStart)
	advanceTabRuntime(t, runtime, tb, runtime.Now()+3*animation.TickRate)
	settlingMiddle := tb.GetDragLayerInfo(100, 4)
	require.NotNil(t, settlingMiddle)
	assert.NotEqual(t, settlingStart.X, settlingMiddle.X, "drop layer must visibly settle toward its final stable-ID slot")

	advanceTabRuntime(t, runtime, tb, runtime.Now()+reorderAnimDuration+time.Millisecond)
	for i := 0; i < 20 && tb.IsAnimating(); i++ {
		advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.TickRate)
	}
	assert.False(t, tb.HasFloatingOverlay())
	assert.Zero(t, runtime.ActiveCount(), "runtime must idle after drag and settle animations complete")
}

func TestActiveDragReleaseOutsideStillUsesLastInsertionTarget(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(80)
	tabs := motionTabs(3, 0)
	tb.SetTabs(tabs, 0)
	tb.View()

	source := tb.dragBounds[0]
	grabX := source.start + 1
	tb.Update(tea.MouseClickMsg{X: grabX, Button: tea.MouseLeft})
	tb.Update(DragHoldMsg{seq: tb.drag.seq})
	require.True(t, tb.drag.active)
	require.Len(t, tb.dragBounds, 2, "hold update installs only the two in-strip targets before View")
	for _, bound := range tb.dragBounds {
		assert.NotEqual(t, tabs[0].SessionID, bound.sessionID, "floating source is not an in-strip target")
	}

	last := boundForSession(t, tb, tabs[2].SessionID)
	outsideX := tb.width + last.end
	tb.Update(tea.MouseMotionMsg{X: outsideX})
	msgs := commandMessages(tb.Update(tea.MouseReleaseMsg{X: outsideX, Button: tea.MouseLeft}))
	require.Len(t, msgs, 1)
	assert.Equal(t, messages.ReorderTabMsg{FromIdx: 0, ToIdx: 2}, msgs[0])
	assert.False(t, tb.IsDragging())
}

func TestDelayedRevealAndManualWheelCancellation(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(30)
	tb.SetTabs(motionTabs(8, 0), 0)
	tb.View()
	before := tb.scrollOffset
	tb.SetTabs(motionTabs(8, 7), 7)
	assert.True(t, tb.scrollPending)
	tb.View()
	assert.Equal(t, before, tb.scrollOffset, "reveal must not snap before delay")
	tb.Update(ScrollDelayMsg{seq: tb.scrollSeq})
	assert.True(t, tb.scrollAnim.Running())
	tb.Update(messages.WheelCoalescedMsg{Delta: 1})
	assert.False(t, tb.scrollAnim.Running())
	assert.Greater(t, tb.scrollOffset, before)
}

func TestStopAnimationsCancelsTransitionsAndReleasesSubscription(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tb.SetWidth(30)
	tabs := motionTabs(8, 0)
	tabs[0].IsRunning = true
	require.NotNil(t, tb.SetTabs(tabs, 0))
	tb.scrollAnim.Start(scrollAnimDuration, animation.EaseOutQuint)
	tb.reorderAnim.Start(reorderAnimDuration, animation.EaseOutQuint)
	tb.dragAnim.Start(dragReflowAnimDuration, animation.EaseOutQuint)
	tb.scrollPending = true
	tb.drag = dragState{active: true}
	require.Positive(t, runtime.ActiveCount())

	tb.StopAnimations()

	assert.False(t, tb.IsAnimating())
	assert.False(t, tb.IsDragging())
	assert.False(t, tb.scrollPending)
	assert.False(t, tb.indicatorSub.IsActive())
	assert.Zero(t, runtime.ActiveCount())
	tb.StopAnimations()
	assert.Zero(t, runtime.ActiveCount())
}

func TestAttentionWinsRunningAndQuiesces(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 6)
	tabs := motionTabs(2, 0)
	tabs[0].IsRunning = true
	tabs[0].NeedsAttention = true
	tabs[1].NeedsAttention = true
	tb.SetWidth(40)
	tb.SetTabs(tabs, 0)

	view := ansi.Strip(tb.View())
	require.Empty(t, busyGlyph(view), "attention retains target precedence over running")
	assert.Contains(t, view, "! ", "idle attention tab keeps its static marker")
	assert.Zero(t, runtime.ActiveCount())

	tabs[0].IsRunning = false
	tb.SetTabs(tabs, 0)
	view = ansi.Strip(tb.View())
	assert.Empty(t, busyGlyph(view))
	assert.Equal(t, 2, strings.Count(view, "! "))
	assert.Zero(t, runtime.ActiveCount(), "static attention markers must not keep ticks alive")
	assert.Nil(t, runtime.Continue())
}

func TestViewCacheAndMouseControls(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	tb.SetWidth(80)
	tabs := motionTabs(3, 0)
	tabs[1].IsRunning = true
	tb.SetTabs(tabs, 0)
	first := tb.View()
	assert.Equal(t, first, tb.View())

	// Middle-close and add retain their existing message contracts.
	bound := tb.dragBounds[1]
	msgs := commandMessages(tb.Update(tea.MouseClickMsg{X: bound.start + 1, Button: tea.MouseMiddle}))
	require.Len(t, msgs, 1)
	assert.Equal(t, "b", msgs[0].(messages.CloseTabMsg).SessionID)
	var plus clickZone
	for _, z := range tb.zones {
		if z.isPlus {
			plus = z
			break
		}
	}
	msgs = commandMessages(tb.handleClick(plus.startX))
	require.Len(t, msgs, 1)
	_, ok := msgs[0].(messages.SpawnSessionMsg)
	assert.True(t, ok)
}

func plusZone(tb *TabBar) clickZone {
	for _, z := range tb.zones {
		if z.isPlus {
			return z
		}
	}
	return clickZone{}
}

func TestNewButtonFollowsTabsUntilOverflow(t *testing.T) {
	tb := New(newMotionRuntime(), 4)
	tabs := motionTabs(2, 0)
	tabWidth := FixedTabWidth(4)
	for _, tc := range []struct {
		name      string
		width     int
		wantPlusX int
		wantArrow bool
	}{
		{name: "exact fit", width: 2*tabWidth + plusButtonWidth, wantPlusX: 2 * tabWidth},
		{name: "spare space", width: 2*tabWidth + plusButtonWidth + 5, wantPlusX: 2 * tabWidth},
		{name: "one cell overflow", width: 2*tabWidth + plusButtonWidth - 1, wantPlusX: 2*tabWidth + plusButtonWidth - 1 - plusButtonWidth, wantArrow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb.SetWidth(tc.width)
			tb.SetTabs(tabs, 0)
			assert.Equal(t, tc.width, lipgloss.Width(tb.View()))
			assert.Equal(t, tc.wantPlusX, plusZone(tb).startX)
			var arrow bool
			for _, z := range tb.zones {
				arrow = arrow || z.isScrollRight
			}
			assert.Equal(t, tc.wantArrow, arrow)
		})
	}
}

func TestRenderedGeometryClipsUnicodeAndControlsHits(t *testing.T) {
	tb := New(newMotionRuntime(), 6)
	tabs := []messages.TabInfo{{SessionID: "wide", Title: "界界界", IsActive: true}, {SessionID: "next", Title: "next"}}
	tb.SetWidth(FixedTabWidth(6) + plusButtonWidth + scrollArrowWidth)
	tb.SetTabs(tabs, 0)
	tb.scrollOffset = 3
	tb.InvalidateCache()
	tb.installGeometry()
	tb.View()
	require.NotEmpty(t, tb.dragBounds)
	for _, b := range tb.dragBounds {
		assert.GreaterOrEqual(t, b.start, 0)
		assert.LessOrEqual(t, b.end, plusZone(tb).startX)
	}
	assert.Empty(t, commandMessages(tb.handleClick(plusZone(tb).startX-1)), "clipped empty cells must not hit hidden tabs")
	msgs := commandMessages(tb.handleMiddleClick(tb.dragBounds[len(tb.dragBounds)-1].start))
	require.Len(t, msgs, 1)
	assert.Equal(t, tb.dragBounds[len(tb.dragBounds)-1].sessionID, msgs[0].(messages.CloseTabMsg).SessionID)
}

func TestDirectionalReflowRetargetsFromCurrentRenderedX(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 8)
	tb.SetWidth(100)
	tb.SetTabs(motionTabs(5, 0), 0)
	tb.View()
	src := tb.dragBounds[2]
	tb.drag = dragState{active: true, dragIdx: 2, dropIdx: 2, cursorX: src.start + 1, grabOffset: 1}
	tb.updateDragReflow()
	right := boundForSession(t, tb, "d")
	tb.handleMouseMotion((right.start+right.end)/2 + tb.drag.grabOffset)
	advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.TickRate)
	current := tb.dragPreviewOffset("d")
	require.NotZero(t, current)

	left := boundForSession(t, tb, "b")
	tb.handleMouseMotion((left.start+left.end)/2 + tb.drag.grabOffset - 1)
	assert.Equal(t, current, tb.dragOffsetFrom["d"], "reversal must retarget from current rendered X")
	assert.Zero(t, tb.dragOffsetTo["d"])
	assert.True(t, tb.dragAnim.Running())
}

func TestDropSettlesContinuouslyWithAndWithoutReorder(t *testing.T) {
	for _, reorder := range []bool{false, true} {
		t.Run(map[bool]string{false: "no reorder", true: "reorder"}[reorder], func(t *testing.T) {
			runtime := newMotionRuntime()
			tb := New(runtime, 8)
			tb.SetWidth(100)
			tabs := motionTabs(4, 0)
			tb.SetTabs(tabs, 0)
			tb.View()
			src := tb.dragBounds[1]
			dropIdx, releaseX := 1, src.start+5
			if reorder {
				dropIdx, releaseX = 4, tb.dragBounds[3].end-3
			}
			tb.drag = dragState{active: true, dragIdx: 1, dropIdx: dropIdx, cursorX: releaseX + 2, grabOffset: 2}
			pre := tb.GetDragLayerInfo(120, 2).X
			cmd := tb.handleMouseRelease(releaseX + 2)
			post := tb.GetDragLayerInfo(120, 2)
			require.NotNil(t, post)
			assert.Equal(t, pre, post.X, "first post-release frame must preserve floating X")
			if reorder {
				require.NotNil(t, cmd)
				tb.SetTabs([]messages.TabInfo{tabs[0], tabs[2], tabs[3], tabs[1]}, 0)
			}
			start, target := float64(post.X), tb.settlingDrop.targetX
			advanceTabRuntime(t, runtime, tb, runtime.Now()+3*animation.TickRate)
			middle := tb.GetDragLayerInfo(120, 2)
			require.NotNil(t, middle)
			assert.GreaterOrEqual(t, float64(middle.X), min(start, target))
			assert.LessOrEqual(t, float64(middle.X), max(start, target))
			advanceTabRuntime(t, runtime, tb, runtime.Now()+reorderAnimDuration+time.Millisecond)
			assert.Nil(t, tb.GetDragLayerInfo(120, 2))
			assert.Zero(t, runtime.ActiveCount())
		})
	}
}

func TestSettlingReducerRetargetsMutationsFromCurrentFloatAndEndsInvalidID(t *testing.T) {
	for _, mutate := range []struct {
		name string
		do   func(*TabBar, []messages.TabInfo)
	}{
		{name: "resize", do: func(tb *TabBar, _ []messages.TabInfo) { tb.SetWidth(35) }},
		{name: "title width", do: func(tb *TabBar, _ []messages.TabInfo) { tb.SetMaxTitleLength(5) }},
		{name: "busy and title", do: func(tb *TabBar, tabs []messages.TabInfo) {
			tabs[3].IsRunning = true
			tabs[3].Title = "renamed while settling"
			tb.SetTabs(tabs, 0)
		}},
		{name: "reveal scroll correction", do: func(tb *TabBar, _ []messages.TabInfo) {
			tb.Update(messages.WheelCoalescedMsg{Delta: 2})
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			runtime := newMotionRuntime()
			tb := New(runtime, 8)
			tabs := motionTabs(7, 0)
			tb.SetWidth(42)
			tb.SetTabs(tabs, 0)
			tb.View()
			tb.beginSettlingDrop(tabs[3].SessionID, 4, 30)
			advanceTabRuntime(t, runtime, tb, runtime.Now()+2*animation.TickRate)
			before := tb.settlingFloatX()
			mutate.do(tb, tabs)
			if tb.settlingDrop == nil { // exact target after relayout is the only legal snap.
				return
			}
			assert.InDelta(t, before, tb.settlingDrop.currentX, 1.0,
				"geometry retarget must start from the interpolated float")
		})
	}

	tb := New(newMotionRuntime(), 8)
	tabs := motionTabs(4, 0)
	tb.SetWidth(50)
	tb.SetTabs(tabs, 0)
	tb.beginSettlingDrop(tabs[2].SessionID, 2, 20)
	withoutDragged := append([]messages.TabInfo(nil), tabs[:2]...)
	withoutDragged = append(withoutDragged, tabs[3:]...)
	tb.SetTabs(withoutDragged, 0)
	assert.Nil(t, tb.settlingDrop, "removed stable ID explicitly ends settling")
	assert.False(t, tb.settleAnim.Running())
}

func TestDragTerminationInventory(t *testing.T) {
	for _, tc := range []struct {
		name        string
		termination tea.Msg
		active      bool
		wantSettle  bool
	}{
		{name: "release over tab", termination: tea.MouseReleaseMsg{X: 16, Button: tea.MouseLeft}, active: true, wantSettle: true},
		{name: "release outside", termination: tea.MouseReleaseMsg{X: 200, Button: tea.MouseLeft}, active: true, wantSettle: true},
		{name: "focus loss", termination: tea.BlurMsg{}, active: true, wantSettle: true},
		{name: "other button", termination: tea.MouseClickMsg{X: 16, Button: tea.MouseRight}, active: true, wantSettle: true},
		{name: "before hold boundary", termination: tea.MouseReleaseMsg{X: 16, Button: tea.MouseLeft}, active: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb := New(newMotionRuntime(), 8)
			tb.SetWidth(45)
			tb.SetTabs(motionTabs(5, 0), 0)
			tb.View()
			src := tb.dragBounds[1]
			tb.drag = dragState{pending: !tc.active, active: tc.active, dragIdx: 1, dropIdx: 3, startX: src.start + 1, cursorX: src.start + 8, grabOffset: 1}
			tb.Update(tc.termination)
			assert.False(t, tb.IsDragging())
			assert.Equal(t, tc.wantSettle, tb.settlingDrop != nil)
		})
	}
}

func TestSeededDragStateMachineContinuity(t *testing.T) {
	const steps = 8
	seeds := []int64{0x5eed, 0xc0ffee, 0x575f1e35}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed_%x", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			for step := range steps {
				runtime := newMotionRuntime()
				tb := New(runtime, 4+rng.Intn(8))
				n := 3 + rng.Intn(7)
				tabs := motionTabs(n, rng.Intn(n))
				tb.SetWidth(22 + rng.Intn(70))
				tb.SetTabs(tabs, 0)
				tb.scrollOffset = rng.Intn(10)
				tb.InvalidateCache()
				tb.installGeometry()
				tb.View()
				srcIdx := rng.Intn(n)
				var src tabBound
				for _, b := range tb.dragBounds {
					if b.tabIdx == srcIdx {
						src = b
					}
				}
				if src.sessionID == "" { // fully clipped source cannot be grabbed.
					continue
				}
				dropIdx := rng.Intn(n + 1)
				releaseX := -20 + rng.Intn(tb.width+40)
				tb.drag = dragState{active: true, dragIdx: srcIdx, dropIdx: dropIdx, cursorX: releaseX, grabOffset: rng.Intn(max(1, src.end-src.start))}
				pre := tb.GetDragLayerInfo(tb.width, 0).X
				cmd := tb.handleMouseRelease(releaseX)
				post := tb.GetDragLayerInfo(tb.width, 0)
				if post == nil { // equal positions intentionally snap directly idle.
					continue
				}
				require.Equal(t, pre, post.X, "seed=%d step=%d first release frame", seed, step)

				if cmd != nil {
					msgs := commandMessages(cmd)
					if len(msgs) == 1 {
						r := msgs[0].(messages.ReorderTabMsg)
						moved := append([]messages.TabInfo(nil), tabs...)
						tab := moved[r.FromIdx]
						moved = append(moved[:r.FromIdx], moved[r.FromIdx+1:]...)
						moved = append(moved, messages.TabInfo{})
						copy(moved[r.ToIdx+1:], moved[r.ToIdx:])
						moved[r.ToIdx] = tab
						if rng.Intn(2) == 0 { // release immediately before or after a tick.
							advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.TickRate)
						}
						tb.SetTabs(moved, 0)
					}
				}

				if tb.settlingDrop == nil {
					continue
				}
				last := tb.settlingX()
				target := int(tb.settlingDrop.targetX + 0.5)
				for frame := 0; frame < 30 && tb.settlingDrop != nil; frame++ {
					advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.TickRate)
					if tb.settlingDrop == nil {
						break
					}
					x := tb.settlingX()
					assert.GreaterOrEqual(t, x, min(last, target), "seed=%d step=%d", seed, step)
					assert.LessOrEqual(t, x, max(last, target), "seed=%d step=%d", seed, step)
					last = x
				}
				assert.False(t, tb.settleAnim.Running())
			}
		})
	}
}

func busyGlyph(view string) string {
	plain := ansi.Strip(view)
	for _, glyph := range animation.Card.Frames() {
		if strings.Contains(plain, glyph) {
			return glyph
		}
	}
	return ""
}

func TestPressHoldMotionReleasePublishesEveryLiveOverlayFrameThenQuiesces(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 10)
	tb.SetWidth(100)
	tabs := motionTabs(4, 0)
	tb.SetTabs(tabs, 0)
	tb.View()

	source := tb.dragBounds[1]
	x := source.start + 2
	tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})
	tb.Update(DragHoldMsg{seq: tb.drag.seq})
	require.True(t, tb.TakeVisualDirty(), "drag activation must publish the floating layer")

	target := boundForSession(t, tb, "c")
	x = (target.start+target.end)/2 + tb.drag.grabOffset + 1
	tb.Update(tea.MouseMotionMsg{X: x})
	require.True(t, tb.TakeVisualDirty(), "lossless mouse motion must publish its live position")

	advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.TickRate)
	require.True(t, tb.TakeVisualDirty(), "bystander reflow must publish an intermediate frame")

	_ = tb.Update(tea.MouseReleaseMsg{X: x, Button: tea.MouseLeft})
	require.True(t, tb.TakeVisualDirty(), "release must publish the start of drop settling")
	reordered := []messages.TabInfo{tabs[0], tabs[2], tabs[1], tabs[3]}
	tb.SetTabs(reordered, 0)
	start := tb.GetDragLayerInfo(100, 2)
	require.NotNil(t, start)
	advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.TickRate)
	middle := tb.GetDragLayerInfo(100, 2)
	require.NotNil(t, middle)
	require.NotEqual(t, start.X, middle.X)
	require.True(t, tb.TakeVisualDirty(), "drop overlay motion must dirty root even when the tab row is unchanged")

	advanceTabRuntime(t, runtime, tb, runtime.Now()+reorderAnimDuration+animation.TickRate)
	assert.False(t, tb.HasFloatingOverlay())
	assert.Zero(t, runtime.ActiveCount())
	assert.Nil(t, runtime.Continue())
}

func TestBusyTickReportsOnlyVisibleFrameChanges(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 6)
	tabs := motionTabs(2, 0)
	tabs[1].IsRunning = true
	tb.SetWidth(40)
	tb.SetTabs(tabs, 0)
	first := tb.View()

	advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.TickRate)
	assert.False(t, tb.TakeVisualDirty(), "sub-frame tick must not dirty unchanged output")
	assert.Equal(t, first, tb.View())

	advanceTabRuntime(t, runtime, tb, runtime.Now()+8*animation.TickRate)
	assert.True(t, tb.TakeVisualDirty(), "busy-frame change must reach the visual dirty contract")
	assert.NotEqual(t, busyGlyph(first), busyGlyph(tb.View()))
	assert.False(t, tb.TakeVisualDirty(), "visual dirty is consumed exactly once")
}

func TestBusyTabsShareOneSubscriptionAndStableGeometry(t *testing.T) {
	runtime := newMotionRuntime()
	tb := New(runtime, 6)
	tabs := motionTabs(5, 0)
	for i := range tabs {
		tabs[i].IsRunning = true
	}
	tb.SetWidth(25)
	tb.SetTabs(tabs, 0)
	firstGlyph := busyGlyph(tb.View())
	require.NotEmpty(t, firstGlyph)
	assert.Equal(t, int32(1), runtime.ActiveCount())
	for _, b := range tb.dragBounds {
		assert.LessOrEqual(t, b.end-b.start, FixedTabWidth(6))
	}
	before := append([]tabBound(nil), tb.dragBounds...)
	advanceTabRuntime(t, runtime, tb, runtime.Now()+animation.Card.DefaultFrameDuration())
	secondGlyph := busyGlyph(tb.View())
	require.NotEmpty(t, secondGlyph)
	assert.NotEqual(t, firstGlyph, secondGlyph)
	assert.Equal(t, animation.Card.FrameAt(runtime.Now()), secondGlyph)
	assert.Equal(t, before, tb.dragBounds)
	for i := range tabs {
		tabs[i].IsRunning = false
	}
	tb.SetTabs(tabs, 0)
	assert.Zero(t, runtime.ActiveCount())
}

type motionScheduler struct{ now time.Time }

func (s *motionScheduler) Now() time.Time { return s.now }
func (s *motionScheduler) Tick(delay time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(delay)
		return create(s.now)
	}
}

func newMotionRuntime() *animation.Runtime {
	return animation.NewRuntimeWithScheduler(&motionScheduler{now: time.Unix(1, 0)})
}
