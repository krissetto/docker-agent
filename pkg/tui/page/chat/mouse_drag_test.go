package chat

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestMessagesScrollbarDragDoesNotHoverSidebarSubagent(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.height = 24
	p.sidebar.Update(&runtime.SubagentTreeEvent{Snapshot: subagent.Snapshot{
		Root: "root:sess",
		Nodes: []subagent.NodeSnapshot{
			{
				Node: subagent.Node{ID: "root:sess", Agent: "root", State: subagent.NodeRunning},
				Children: []subagent.NodeSnapshot{
					{
						Node: subagent.Node{
							ID: "a1b2c", Agent: "coder", Parent: "root:sess",
							State: subagent.NodeRunning, CreatedAt: time.Now().Add(-time.Minute),
						},
					},
				},
			},
		},
	}})
	p.SetSize(p.width, p.height)

	// Render once to register the sidebar's row geometry, then prove ordinary
	// motion at the sidebar's actual X still activates the row hover.
	coderY := renderedLineContaining(t, p.sidebar.View(), "coder")
	sl := p.computeSidebarLayout()
	sidebarX := styles.AppPadding + sl.sidebarStartX
	coderLine := strings.Split(ansi.Strip(p.sidebar.View()), "\n")[coderY]
	prefix, _, found := strings.Cut(coderLine, "coder")
	require.True(t, found)
	coderX := ansi.StringWidth(prefix)
	click, nodeID := p.sidebar.HandleClickType(coderX, coderY)
	require.Equal(t, sidebar.ClickSubagent, click)
	require.Equal(t, "a1b2c", nodeID, "the actual name cell routes to the canonical node ID")
	p.handleMouseMotion(tea.MouseMotionMsg{X: sidebarX + coderX, Y: coderY})
	require.Contains(t, ansi.Strip(p.sidebar.View()), "coder (a1b2c)",
		"ordinary motion over the rendered name must establish hover before drag capture")

	// Fill the chat, start a real messages-scrollbar thumb drag, and move it to
	// the same absolute Y as the sidebar row. The drag capture path broadcasts
	// motion to both components, but must not let that chat-coordinate motion
	// masquerade as a sidebar hover.
	for range 30 {
		p.messages.AddUserMessage(strings.Repeat("chat line\n", 3))
	}
	p.messages.View()
	messagesScrollbarX := styles.AppPadding + sl.chatStartX + sl.chatWidth - 1
	thumbY := sl.chatHeight - 1
	require.True(t, p.messages.IsMouseOnScrollbar(messagesScrollbarX, thumbY), "messages should expose a scrollbar")
	p.handleMouseClick(tea.MouseClickMsg{X: messagesScrollbarX, Y: thumbY, Button: tea.MouseLeft})
	require.True(t, p.messages.IsScrollbarDragging())

	p.handleMouseMotion(tea.MouseMotionMsg{X: messagesScrollbarX, Y: coderY, Button: tea.MouseLeft})
	assert.True(t, p.messages.IsScrollbarDragging(), "captured messages drag must continue")
	assert.NotContains(t, ansi.Strip(p.sidebar.View()), "coder (a1b2c)",
		"chat scrollbar motion must not hover a sidebar row at the same absolute Y")
}

func TestSidebarScrollbarDragRetainsCaptureOutsideSidebar(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.height = 12
	queued := make([]string, 30)
	for i := range queued {
		queued[i] = "queued message"
	}
	p.sidebar.SetQueuedMessages(queued...)
	p.SetSize(p.width, p.height)
	p.sidebar.View()

	sl := p.computeSidebarLayout()
	scrollbarX := styles.AppPadding + sl.sidebarStartX + sl.sidebarWidth - toggleColumnWidth - 1
	thumbY := 0
	p.handleMouseClick(tea.MouseClickMsg{X: scrollbarX, Y: thumbY, Button: tea.MouseLeft})
	require.True(t, p.sidebar.IsScrollbarDragging())

	p.handleMouseMotion(tea.MouseMotionMsg{X: styles.AppPadding + sl.chatStartX, Y: 0, Button: tea.MouseLeft})
	assert.True(t, p.sidebar.IsScrollbarDragging(), "captured sidebar drag must continue outside its bounds")
}

func renderedLineContaining(t *testing.T, view, want string) int {
	t.Helper()
	for y, line := range strings.Split(ansi.Strip(view), "\n") {
		if strings.Contains(line, want) {
			return y
		}
	}
	t.Fatalf("rendered view does not contain %q:\n%s", want, ansi.Strip(view))
	return -1
}
