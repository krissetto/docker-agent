package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// messagesScrollbarOwner is optional so embedders retain the existing Page
// interface. Only a real thumb press grants this root-level pointer capture.
type messagesScrollbarOwner interface {
	IsMessagesScrollbarDragging() bool
	CancelMessagesScrollbarDrag()
}

type messagesScrollbarCapture struct {
	page      chat.Page
	sessionID string
	owner     messagesScrollbarOwner
	canceled  bool
	width     int
	height    int
	content   int
	lean      bool
	pane      splitRect
	shell     chat.SplitShellGeometry
}

func (m *appModel) captureMessagesScrollbar() {
	if m.messagesScrollbar != nil {
		return
	}
	m.capturePaneMessagesScrollbar(m.paneFocus(), m.chatPage)
}

func (m *appModel) capturePaneMessagesScrollbar(id string, page chat.Page) {
	owner, ok := page.(messagesScrollbarOwner)
	if !ok || !owner.IsMessagesScrollbarDragging() {
		return
	}
	m.messagesScrollbar = &messagesScrollbarCapture{
		page: page, sessionID: id, owner: owner,
		width: m.width, height: m.height, content: m.contentHeight,
		lean: m.leanMode, pane: m.paneGeometry.Panes[id], shell: m.paneShell,
	}
}

func (m *appModel) cancelMessagesScrollbar() {
	capture := m.messagesScrollbar
	if capture == nil || capture.canceled {
		return
	}
	capture.owner.CancelMessagesScrollbarDrag()
	capture.canceled = true
	m.viewCacheValid = false
	// Keep the canceled capture until release: that event must not fall
	// through to a new page, dialog, editor selection or tab interaction.
}

func (m *appModel) validateMessagesScrollbar() {
	capture := m.messagesScrollbar
	if capture == nil || capture.canceled {
		return
	}
	if m.tickPaused || m.dialogMgr.Open() || m.err != nil ||
		m.chatPages[capture.sessionID] != capture.page || !m.paneVisible(capture.sessionID) ||
		m.width != capture.width || m.height != capture.height || m.contentHeight != capture.content ||
		m.leanMode != capture.lean || m.paneGeometry.Panes[capture.sessionID] != capture.pane || m.paneShell != capture.shell ||
		!capture.owner.IsMessagesScrollbarDragging() {
		m.cancelMessagesScrollbar()
	}
}

// routeMessagesScrollbar retains the originating session and global coordinate
// space even over another pane or outside the terminal. It runs before ordinary
// region dispatch, including modal and composer handlers.
func (m *appModel) routeMessagesScrollbar(msg tea.Msg, release bool) (tea.Cmd, bool) {
	if m.messagesScrollbar == nil {
		return nil, false
	}
	m.validateMessagesScrollbar()
	capture := m.messagesScrollbar
	if release {
		if msg.(tea.MouseReleaseMsg).Button != tea.MouseLeft {
			return nil, true
		}
		m.messagesScrollbar = nil
	}
	if capture.canceled {
		return nil, true
	}
	before := capture.page.VisualGeneration()
	updated, cmd := capture.page.Update(msg)
	page := updated.(chat.Page)
	m.chatPages[capture.sessionID] = page
	if m.paneFocus() == capture.sessionID {
		m.chatPage = page
	}
	if page.VisualGeneration() != before {
		m.viewCacheValid = false
	}
	return m.routePaneCmd(capture.sessionID, cmd), true
}
