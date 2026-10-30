package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

type scrollbarPointerState struct {
	x, y  int
	known bool
}

type scrollbarPointerOwner interface {
	SetScrollbarPointer(x, y int)
}

// Hover observes routing without forwarding a second drag or selection event.
func (m *appModel) syncScrollbarPointer(msg tea.Msg) {
	if m.leanMode {
		return
	}
	switch event := msg.(type) {
	case tea.MouseMsg:
		m.scrollbarPointer = scrollbarPointerState{x: event.Mouse().X, y: event.Mouse().Y, known: true}
	case messages.WheelCoalescedMsg:
		m.scrollbarPointer = scrollbarPointerState{x: event.X, y: event.Y, known: true}
	case tea.BlurMsg:
		m.scrollbarPointer.known = false
	}
	x, y := -1, -1
	if m.scrollbarPointer.known {
		x, y = m.scrollbarPointer.x, m.scrollbarPointer.y
	}
	if owner, ok := m.dialogMgr.(interface {
		SetScrollbarActive(active bool) bool
		SetScrollbarPointer(x, y int) bool
	}); ok {
		dialogActive := !m.paneHidden && !m.blurred && m.err == nil && m.dialogMgr.Open()
		changed := owner.SetScrollbarActive(dialogActive)
		dialogX, dialogY := -1, -1
		if dialogActive {
			dialogX, dialogY = x, y
		}
		if owner.SetScrollbarPointer(dialogX, dialogY) || changed {
			m.viewCacheValid = false
		}
	}
	occluded := m.paneHidden || m.blurred || m.dialogMgr.Open() || m.err != nil
	chatX, chatY := x, y
	if occluded || m.reviewOwnsPointer(x, y) || m.isDragging || m.paneGesture != nil {
		chatX, chatY = -1, -1
	}
	updatePage := func(page chat.Page) {
		owner, ok := page.(scrollbarPointerOwner)
		if !ok {
			return
		}
		before, sidebarBefore := page.VisualGeneration(), sidebarVisualGeneration(page)
		if visibility, ok := page.(interface{ SetScrollbarsActive(active bool) }); ok {
			visibility.SetScrollbarsActive(!occluded && (m.reviewSurface() == nil || m.reviewDocked()))
		}
		owner.SetScrollbarPointer(chatX, chatY)
		if before != page.VisualGeneration() || sidebarBefore != sidebarVisualGeneration(page) {
			m.viewCacheValid = false
		}
	}
	updatePage(m.chatPage)
	for _, id := range m.panes.Sessions() {
		if id != m.paneFocus() && m.paneVisible(id) {
			updatePage(m.chatPages[id])
		}
	}
	if occluded {
		x, y = -1, -1
	}
	for model := range m.reviewAnimations {
		if model.SetScrollbarPointer(x, y) {
			m.viewCacheValid = false
		}
	}
}
