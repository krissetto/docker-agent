package chat

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/messages"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// chatPaneCountingMessages counts View calls on the transcript child.
type chatPaneCountingMessages struct {
	messages.Model

	views int
}

func (m *chatPaneCountingMessages) View() string { m.views++; return m.Model.View() }

// chatPaneUncachedVertical composes the vertical layout without the pane cache.
func chatPaneUncachedVertical(p *chatPage) string {
	sl := p.computeSidebarLayout()
	chatView := styles.ChatStyle.Height(sl.chatHeight).Width(sl.chatWidth).Render(p.messagesView(sl))
	toggle := p.renderSidebarHandle(sl.chatHeight)
	sidebarView := lipgloss.NewStyle().Width(sl.sidebarWidth-toggleColumnWidth).Height(sl.chatHeight).Align(lipgloss.Left, lipgloss.Top).Render(p.sidebar.View())
	var body string
	if sl.sidebarOnLeft {
		body = lipgloss.JoinHorizontal(lipgloss.Left, sidebarView, toggle, chatView)
	} else {
		body = lipgloss.JoinHorizontal(lipgloss.Left, chatView, toggle, sidebarView)
	}
	return styles.AppStyle.Height(p.height).Render(body)
}

// The cached pane must match the uncached composition after each invalidation trigger.
func TestChatPaneCacheMatchesUncachedVerticalView(t *testing.T) {
	t.Parallel()
	for _, position := range []msgtypes.SidebarPosition{msgtypes.SidebarRight, msgtypes.SidebarLeft} {
		t.Run(string(position), func(t *testing.T) {
			p := newLayoutTestPage(t, position)
			p.SetSize(p.width, p.height)
			child := &chatPaneCountingMessages{Model: p.messages}
			p.messages = child
			p.messages.AddUserMessage("first message")

			require.Equal(t, chatPaneUncachedVertical(p), p.View())
			require.True(t, p.chatPaneCache.valid)
			cached := p.chatPaneCache.rendered
			viewsBefore := child.views
			require.Equal(t, chatPaneUncachedVertical(p), p.View())
			require.Equal(t, cached, p.chatPaneCache.rendered, "unchanged transcript reuses the pane")
			require.Greater(t, child.views, viewsBefore, "child View side effects must still run")

			p.messages.AddUserMessage("new message")
			require.Equal(t, chatPaneUncachedVertical(p), p.View())
			require.NotEqual(t, cached, p.chatPaneCache.rendered)

			p.SetSize(140, 20)
			require.Equal(t, chatPaneUncachedVertical(p), p.View())

			_, _ = p.Update(msgtypes.ThemeChangedMsg{})
			require.False(t, p.chatPaneCache.valid)
			require.Equal(t, chatPaneUncachedVertical(p), p.View())
		})
	}
}
