package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/components/notification"
)

func (m *appModel) returnToPreviousSession() (tea.Model, tea.Cmd) {
	for len(m.previousSessions) > 0 {
		last := len(m.previousSessions) - 1
		id := m.previousSessions[last]
		m.previousSessions = m.previousSessions[:last]
		if id == "" || id == m.paneFocus() || m.supervisor.GetRunner(id) == nil {
			continue
		}
		history := append([]string(nil), m.previousSessions...)
		model, cmd := m.handleSwitchTab(id)
		m.previousSessions = history
		return model, cmd
	}
	return m, notification.InfoCmd("No previous open session")
}

func (m *appModel) showSubagentSessions() tea.Cmd {
	if m.application == nil || m.application.Session() == nil {
		return notification.InfoCmd("No active session")
	}
	return m.openSubagentsTree()
}
