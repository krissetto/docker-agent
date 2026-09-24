package sessionbrowser

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func (m *Model) Update(msg tea.Msg) tea.Cmd {
	defer m.prepare()
	switch msg := msg.(type) {
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft || !m.contains(msg.X, msg.Y) {
			return nil
		}
		m.focused = true
		x, y := msg.X-m.x, msg.Y-m.y
		if m.renaming != "" {
			return nil
		}
		for _, h := range m.hits {
			if y == h.y && x >= h.x && x < h.x+h.width {
				return m.action(h.action)
			}
		}
		if y >= m.bodyY && y < m.bodyY+m.bodyHeight {
			if handled, cmd := m.scroll.UpdateMouse(msg); handled {
				return cmd
			}
			if x >= m.scroll.ContentWidth() {
				return nil
			}
			line := y - m.bodyY + m.scroll.ScrollOffset()
			if line >= 0 && line < len(m.lineEntries) {
				m.selected = m.entries[m.lineEntries[line]].key
				m.area = treeArea
				m.focusInput()
				return m.open("open")
			}
		}
		return nil
	case tea.MouseWheelMsg:
		if !m.contains(msg.X, msg.Y) || msg.Y < m.y+m.bodyY || msg.Y >= m.y+m.bodyY+m.bodyHeight || m.renaming != "" {
			return nil
		}
		_, cmd := m.scroll.Update(msg)
		return cmd
	case tea.MouseMotionMsg, tea.MouseReleaseMsg:
		if m.scroll.IsDragging() {
			_, cmd := m.scroll.UpdateMouse(msg)
			return cmd
		}
	case messages.WheelCoalescedMsg:
		if m.contains(msg.X, msg.Y) && msg.Y >= m.y+m.bodyY && msg.Y < m.y+m.bodyY+m.bodyHeight && m.renaming == "" {
			_, cmd := m.scroll.Update(msg)
			return cmd
		}
	case tea.KeyPressMsg:
		if !m.focused {
			return nil
		}
		if m.renaming != "" {
			switch msg.String() {
			case "enter":
				return m.commitRename()
			case "esc":
				m.cancelRename()
				return m.focusInput()
			}
			var cmd tea.Cmd
			m.rename, cmd = m.rename.Update(msg)
			m.renameError = ""
			return cmd
		}
		switch msg.String() {
		case "tab":
			m.area = (m.area + 1) % 3
			return m.focusInput()
		case "shift+tab":
			m.area = (m.area + 2) % 3
			return m.focusInput()
		case "ctrl+n":
			return emit(CreateWorkspace{})
		case "ctrl+w":
			if item := m.selectedWorkspace(); item != nil {
				return emit(CloseWorkspace{ID: item.ID})
			}
			return nil
		case "f2":
			return m.beginRename()
		case "ctrl+l":
			return m.loadMore()
		case "esc":
			m.area = treeArea
			return m.focusInput()
		}
		if m.area == searchArea {
			if msg.String() == "enter" || msg.String() == "down" {
				m.area = treeArea
				return m.focusInput()
			}
			return m.updateSearch(msg)
		}
		if msg.String() == "/" {
			m.area = searchArea
			return m.focusInput()
		}
		if m.area == workspaceArea {
			switch msg.String() {
			case "left", "up":
				m.moveWorkspace(-1)
			case "right", "down":
				m.moveWorkspace(1)
			case "enter":
				if item := m.selectedWorkspace(); item != nil {
					return emit(SelectWorkspace{ID: item.ID})
				}
			}
			return nil
		}
		switch msg.String() {
		case "up":
			m.move(-1)
		case "down":
			m.move(1)
		case "home":
			m.selectIndex(0)
		case "end":
			m.selectIndex(len(m.entries) - 1)
		case "pgup":
			m.movePage(-1)
		case "pgdown":
			m.movePage(1)
		case "left":
			m.collapse()
		case "right":
			m.expand()
		case "enter":
			return m.open("open")
		case "ctrl+s":
			return m.open("split")
		case "ctrl+o":
			return m.open("other")
		case "ctrl+r", "delete":
			if e := m.selectedEntry(); e != nil && e.row != nil && e.row.UnavailableReason != "" {
				if msg.String() == "ctrl+r" {
					return emit(RetryMissing{SessionID: e.row.SessionID})
				}
				return emit(RemoveMissing{SessionID: e.row.SessionID})
			}
		}
	case tea.PasteMsg:
		if !m.focused {
			return nil
		}
		if m.renaming != "" {
			var cmd tea.Cmd
			m.rename, cmd = m.rename.Update(msg)
			return cmd
		}
		if m.area == searchArea {
			return m.updateSearch(msg)
		}
	default:
		if !m.focused {
			return nil
		}
		if m.renaming != "" {
			var cmd tea.Cmd
			m.rename, cmd = m.rename.Update(msg)
			return cmd
		}
		if m.area == searchArea {
			return m.updateSearch(msg)
		}
	}
	return nil
}
func (m *Model) updateSearch(msg tea.Msg) tea.Cmd {
	before := m.Query()
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	if m.Query() != before {
		return tea.Batch(cmd, emit(QueryChanged{Query: m.Query()}))
	}
	return cmd
}
func (m *Model) contains(x, y int) bool {
	return x >= m.x && x < m.x+m.width && y >= m.y && y < m.y+m.height
}
func (m *Model) action(action string) tea.Cmd {
	switch action {
	case "search":
		m.area = searchArea
		return m.focusInput()
	case "create":
		return emit(CreateWorkspace{})
	case "rename":
		return m.beginRename()
	case "close":
		if item := m.selectedWorkspace(); item != nil {
			return emit(CloseWorkspace{ID: item.ID})
		}
	case "previous-workspace":
		m.area = workspaceArea
		m.moveWorkspace(-1)
		return m.focusInput()
	case "next-workspace":
		m.area = workspaceArea
		m.moveWorkspace(1)
		return m.focusInput()
	case "select-workspace":
		m.area = workspaceArea
		m.focusInput()
		if item := m.selectedWorkspace(); item != nil {
			return emit(SelectWorkspace{ID: item.ID})
		}
	}
	return nil
}
func (m *Model) moveWorkspace(delta int) {
	if len(m.workspaces) == 0 {
		return
	}
	index := 0
	for i, item := range m.workspaces {
		if item.ID == m.workspaceID {
			index = i
			break
		}
	}
	index = (index + delta + len(m.workspaces)) % len(m.workspaces)
	m.workspaceID = m.workspaces[index].ID
}
func (m *Model) move(delta int) { m.selectIndex(m.selectedIndex() + delta) }
func (m *Model) selectIndex(index int) {
	if len(m.entries) == 0 {
		return
	}
	index = max(0, min(index, len(m.entries)-1))
	m.selected = m.entries[index].key
	m.ensureSelectedVisible()
}
func (m *Model) movePage(direction int) {
	index := m.selectedIndex()
	if index < 0 || len(m.lineEntries) == 0 {
		return
	}
	line := max(0, min(m.starts[index]+direction*max(1, m.bodyHeight), len(m.lineEntries)-1))
	m.selectIndex(m.lineEntries[line])
}
func (m *Model) collapse() {
	e := m.selectedEntry()
	if e == nil {
		return
	}
	if e.row == nil && !e.more && !m.collapsed[e.key] {
		m.collapsed[e.key] = true
		m.rebuild()
		m.prepare()
		m.ensureSelectedVisible()
		return
	}
	if e.parent != "" {
		m.selected = e.parent
		m.ensureSelectedVisible()
	}
}
func (m *Model) expand() {
	e := m.selectedEntry()
	if e == nil || e.row != nil || e.more {
		return
	}
	if m.collapsed[e.key] {
		m.collapsed[e.key] = false
		m.rebuild()
		m.prepare()
	} else {
		m.move(1)
	}
	m.ensureSelectedVisible()
}
