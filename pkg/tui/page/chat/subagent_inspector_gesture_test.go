package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

// Right-click and Ctrl+left on a subagent row open that row's own session
// inspector and never attach, switch, or disclose; plain left still attaches.
func TestSubagentRowInspectorGesturesOpenOwnSessionWithoutNavigation(t *testing.T) {
	cases := []struct {
		name, label, agent, session string
		button                      tea.MouseButton
		mod                         tea.KeyMod
	}{
		{"leaf right-click", "leaf", "leaf", "leaf-session", tea.MouseRight, 0},
		{"leaf ctrl-left", "leaf", "leaf", "leaf-session", tea.MouseLeft, tea.ModCtrl},
		{"branch right-click", "作業planner", "planner", "branch-session", tea.MouseRight, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := newSidebarGesturePage(t, msgtypes.SidebarRight)
			x, y, _ := sidebarGesturePoint(t, p, tc.label)
			_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tc.button, Mod: tc.mod})
			events := runTimerCmd(t, cmd)
			require.Contains(t, events, msgtypes.ShowAgentDetailsMsg{AgentName: tc.agent, SessionID: tc.session})
			requireNoSidebarNavigation(t, events)
		})
	}
}

func TestSubagentRowPlainLeftStillAttachesWithoutInspector(t *testing.T) {
	p, _ := newSidebarGesturePage(t, msgtypes.SidebarRight)
	x, y, _ := sidebarGesturePoint(t, p, "leaf")
	_, cmd := p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	events := runTimerCmd(t, cmd)
	require.Contains(t, events, msgtypes.OpenSubagentMsg{NodeID: "abcde-leaf-full"})
	for _, event := range events {
		if _, ok := event.(msgtypes.ShowAgentDetailsMsg); ok {
			t.Fatalf("plain left opened the inspector: %#v", event)
		}
	}
}
