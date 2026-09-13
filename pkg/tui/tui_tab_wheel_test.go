package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	tuiinput "github.com/docker/docker-agent/pkg/tui/input"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestActualProgramTabWheelRoutingClampAndBoundaryOrder(t *testing.T) {
	root := populateScrollableRoot(t)
	writer := &cacheProgramWriter{}
	coalescer := tuiinput.NewMouseCoalescer()
	t.Cleanup(coalescer.Stop)
	program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(writer), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg { return coalescer.Filter(msg) }))
	coalescer.SetSender(program.Send)
	snapshot := func() shellSnapshot {
		reply := make(chan shellSnapshot, 1)
		program.Send(shellSnapshotMsg{reply: reply})
		select {
		case s := <-reply:
			return s
		case <-time.After(time.Second):
			t.Fatal("tab wheel snapshot timed out")
			return shellSnapshot{}
		}
	}
	tabs := make([]messages.TabInfo, 14)
	for i := range tabs {
		tabs[i] = messages.TabInfo{SessionID: fmt.Sprintf("wheel-tab-%02d", i), Title: fmt.Sprintf("Wheel tab %02d long", i), IsActive: i == 0}
	}
	program.Send(messages.TabsUpdatedMsg{Tabs: tabs, ActiveIdx: 0})
	require.Eventually(t, func() bool { s := snapshot(); return s.active == 0 && strings.Contains(s.tabView, "Wheel tab 00") }, time.Second, time.Millisecond)
	initial := snapshot()
	wheel := func(button tea.MouseButton, y, count int) {
		for range count {
			program.Send(tea.MouseWheelMsg{X: 30, Y: y, Button: button})
		}
		program.Send(tea.MouseReleaseMsg{X: 30, Y: y, Button: tea.MouseLeft})
	}
	wheel(tea.MouseWheelRight, initial.tabY, 2)
	right := snapshot()
	require.NotEqual(t, initial.tabView, right.tabView, "horizontal wheel flushes before release over tabs")
	transcript := func(s shellSnapshot) string { return strings.Join(strings.Split(s.content, "\n")[:s.tabY-1], "\n") }
	require.Equal(t, transcript(initial), transcript(right), "tab wheel never scrolls transcript")
	wheel(tea.MouseWheelLeft, right.tabY, 100)
	left := snapshot()
	require.Equal(t, initial.tabView, left.tabView, "left overscroll clamps at beginning")
	wheel(tea.MouseWheelDown, left.tabY, 2)
	require.NotEqual(t, left.tabView, snapshot().tabView, "vertical fallback scrolls overflow tabs")
	wheel(tea.MouseWheelRight, left.tabY, 100)
	end := snapshot()
	wheel(tea.MouseWheelDown, end.tabY, 100)
	require.Equal(t, end.tabView, snapshot().tabView, "mixed-axis forward overscroll clamps at end")
	wheel(tea.MouseWheelUp, end.tabY, 100)
	require.Equal(t, initial.tabView, snapshot().tabView)
	// Axis and region changes in the same coalescing interval retain ownership.
	program.Send(tea.MouseWheelMsg{X: 30, Y: initial.tabY, Button: tea.MouseWheelRight})
	program.Send(tea.MouseWheelMsg{X: 1, Y: 1, Button: tea.MouseWheelUp})
	program.Send(tea.MouseReleaseMsg{X: 1, Y: 1, Button: tea.MouseLeft})
	mixed := snapshot()
	require.NotEqual(t, initial.tabView, mixed.tabView)
	require.NotEqual(t, transcript(initial), transcript(mixed), "outside vertical wheel still scrolls transcript")
	wheel(tea.MouseWheelRight, 1, 5)
	require.Equal(t, mixed.content, snapshot().content, "horizontal outside tabs is not transcript scrolling")
	wheel(tea.MouseWheelLeft, initial.tabY+initial.tabHeight+1, 5)
	require.Equal(t, mixed.content, snapshot().content, "horizontal editor wheel is inert")
	// Shift+vertical wheel is the same fallback when the terminal reports it.
	program.Send(tea.MouseWheelMsg{X: 30, Y: initial.tabY, Button: tea.MouseWheelDown, Mod: tea.ModShift})
	program.Send(tea.MouseReleaseMsg{X: 30, Y: initial.tabY, Button: tea.MouseLeft})
	require.NotEqual(t, mixed.tabView, snapshot().tabView)
	// Wheel input does not steal an in-flight tab drag or its eventual release.
	wheel(tea.MouseWheelLeft, initial.tabY, 100)
	start := snapshot()
	line := ansi.Strip(strings.Split(start.tabView, "\n")[0])
	x := strings.Index(line, "Wheel tab 00") + tabFrameOrigin() + 2
	require.Greater(t, x, tabFrameOrigin())
	program.Send(tea.MouseClickMsg{X: x, Y: start.tabY, Button: tea.MouseLeft})
	require.Eventually(t, func() bool { return snapshot().overlay }, time.Second, time.Millisecond)
	wheel(tea.MouseWheelRight, start.tabY, 1)
	require.Eventually(t, func() bool { return !snapshot().overlay }, time.Second, time.Millisecond, "release after wheel completes the drag")
	program.Send(messages.TabsUpdatedMsg{Tabs: tabs[:2], ActiveIdx: 0})
	require.Eventually(t, func() bool { return snapshot().active == 0 }, time.Second, time.Millisecond)
	short := snapshot()
	wheel(tea.MouseWheelRight, short.tabY, 100)
	wheel(tea.MouseWheelDown, short.tabY, 100)
	require.Equal(t, short.tabView, snapshot().tabView, "nonoverflow strip does not scroll")
	require.Zero(t, snapshot().active, "wheel does not leave animation leases")
}
