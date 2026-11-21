package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/internal/ansiline"
	"github.com/docker/docker-agent/pkg/tui/internal/resizegrip"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// withSidebarResizeGuide paints over the latest frame without recomposing it.
// The guide is clipped to the chat rows, including the toggle row.
func (m *appModel) withSidebarResizeGuide(view tea.View) tea.View {
	guide, ok := chat.SidebarResizeGuide(m.activeTab.chatPage)
	if !ok || guide.Start >= guide.End {
		m.sidebarGuideCache.content = ""
		return view
	}
	cache := &m.sidebarGuideCache
	if cache.content == "" || cache.guide != guide {
		glyph := resizegrip.RenderGuide(guide.Opacity)
		lines := strings.Split(view.Content, "\n")
		for y := guide.Start; y < min(guide.End, len(lines)); y++ {
			lines[y] = ansiline.Splice(lines[y], glyph, guide.X)
		}
		cache.guide = guide
		cache.content = strings.Join(lines, "\n")
	}
	view.Content = cache.content
	return view
}
