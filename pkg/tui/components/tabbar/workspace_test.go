package tabbar

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestWorkspaceGroupMouseCloseConfirmsWorkspaceButKeyboardClosesFocus(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	t.Cleanup(tb.StopAnimations)
	tb.SetWidth(80)
	tb.SetTabs([]messages.TabInfo{{SessionID: "group", FocusSessionID: "focused-pane", Title: "Group", PaneCount: 3, IsActive: true}}, 0)
	plain := ansi.Strip(tb.View())
	require.Contains(t, plain, "[3]")
	require.Contains(t, plain, "×")
	closeIndex := strings.Index(plain, "×")
	x := ansi.StringWidth(plain[:closeIndex])
	require.Equal(t, []tea.Msg{messages.CloseWorkspaceMsg{SessionID: "group"}}, commandMessages(tb.Update(tea.MouseClickMsg{X: x, Button: tea.MouseLeft})))
	_, hit := tb.TabBodyAt(x, 0)
	require.False(t, hit, "close button cannot start workspace dragging")
	require.Empty(t, commandMessages(tb.Update(tea.MouseClickMsg{X: 2, Button: tea.MouseMiddle})))
	require.Equal(t, []tea.Msg{messages.CloseTabMsg{SessionID: "focused-pane"}}, commandMessages(tb.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})))
	for _, maxTitle := range []int{1, 2, 4, 8} {
		tb.SetMaxTitleLength(maxTitle)
		tb.SetTabs([]messages.TabInfo{{SessionID: "group", PaneCount: 100, Activity: messages.TabActivityRunning}}, 0)
		require.NotEmpty(t, tb.View(), "tiny title budget must never panic")
	}
}

func TestWorkspacePaneInsertionUsesVisibleStripAndRejectsControls(t *testing.T) {
	tb := New(newMotionRuntime(), 8)
	t.Cleanup(tb.StopAnimations)
	tb.SetWidth(80)
	tb.SetTabs(motionTabs(3, 0), 0)
	index, marker, ok := tb.InsertionAt(0, 0)
	require.True(t, ok)
	require.Zero(t, index)
	require.Zero(t, marker)
	index, _, ok = tb.InsertionAt(65, 0)
	require.True(t, ok)
	require.Equal(t, 3, index, "blank strip space appends a detached workspace")
	plain := ansi.Strip(tb.View())
	plusIndex := strings.Index(plain, "+")
	require.NotEqual(t, -1, plusIndex)
	plus := ansi.StringWidth(plain[:plusIndex])
	for _, point := range [][2]int{{plus, 0}, {-1, 0}, {80, 0}, {1, 1}} {
		_, _, ok = tb.InsertionAt(point[0], point[1])
		require.False(t, ok)
	}
	tb.SetWidth(30)
	tb.Update(messages.WheelCoalescedMsg{Delta: 100})
	_, _, ok = tb.InsertionAt(0, 0)
	require.False(t, ok, "overflow arrow is not an insertion target")
}
