package chat

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/internal/resizegrip"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const sidebarGripLength = 3

// IsResizingSidebar reports whether the sidebar captured a resize press, so a
// release over the bottom panel can still be delivered to the sidebar.
func IsResizingSidebar(page Page) bool {
	p, ok := page.(*chatPage)
	return ok && p.isDraggingSidebar
}

// ClearSidebarGripHover handles motion outside chat without ending a drag.
func ClearSidebarGripHover(page Page) tea.Cmd {
	if p, ok := page.(*chatPage); ok {
		return p.sidebarGrip.Set(false, false, p.isDraggingSidebar)
	}
	return nil
}

// ResetSidebarGrip clears the visual state and capture when the page loses pointer access.
func ResetSidebarGrip(page Page) {
	if p, ok := page.(*chatPage); ok {
		p.isDraggingSidebar = false
		p.sidebarGrip.Reset()
	}
}

func (p *chatPage) updateSidebarGrip(x, y int) tea.Cmd {
	sl := p.computeSidebarLayout()
	adjustedX := x - styles.AppPadding
	// Reveal one cell to either side, without taking pointer events from the
	// adjacent pane. Only the divider's painted segment gets direct hover.
	nearEdge := sl.mode == sidebarVertical && y >= 0 && y < sl.chatHeight &&
		adjustedX >= sl.handleX-1 && adjustedX <= sl.handleX+1
	start, end := sidebarGripRows(sl.chatHeight)
	onGrip := sl.isOnHandle(adjustedX) && y >= start && y < end
	return p.sidebarGrip.Set(nearEdge, onGrip, p.isDraggingSidebar)
}

func sidebarGripRows(height int) (start, end int) {
	// Keep row zero exclusively for the existing sidebar toggle.
	start, end = resizegrip.Segment(height-1, sidebarGripLength)
	return 1 + start, 1 + end
}

// renderSidebarHandle renders the toggle and compact grip in their shared
// column. Only the toggle glyph changes with the sidebar's position.
func (p *chatPage) renderSidebarHandle(height int) string {
	if height <= 0 {
		return ""
	}

	expandGlyph, collapseGlyph := "«", "»"
	if p.layoutSettings.SidebarPosition == msgtypes.SidebarLeft {
		expandGlyph, collapseGlyph = "»", "«"
	}
	glyph := collapseGlyph
	if p.sidebar.IsCollapsed() {
		glyph = expandGlyph
	}

	start, end := sidebarGripRows(height)
	return styles.MutedStyle.Render(glyph) + strings.Repeat("\n ", start-1) +
		strings.Repeat("\n"+p.sidebarGrip.Render("┃", 1), end-start) +
		strings.Repeat("\n ", height-end)
}
