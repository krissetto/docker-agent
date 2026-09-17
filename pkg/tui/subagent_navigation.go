package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
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
	snapshot := m.application.Session().GetSubagentTree()
	if source, ok := m.application.Runtime().(interface{ SubagentTree() *subagent.Tree }); ok && source.SubagentTree() != nil {
		live := source.SubagentTree().Snapshot()
		snapshot = &live
	}
	if snapshot == nil {
		return notification.InfoCmd("No subagent sessions available")
	}
	var items []commands.Item
	var visit func([]subagent.NodeSnapshot)
	visit = func(nodes []subagent.NodeSnapshot) {
		for _, item := range nodes {
			id := string(item.Node.ID)
			items = append(items, commands.Item{
				ID: "subagent." + id, Label: item.Node.DisplayName() + " (" + id + ")",
				Description: "Open canonical live session; send only when you explicitly submit", Category: "Subagents",
				Execute: func(string) tea.Cmd { return core.CmdHandler(messages.OpenSubagentMsg{NodeID: id}) },
			})
			visit(item.Children)
		}
	}
	visit(snapshot.Nodes)
	if len(items) == 0 {
		return notification.InfoCmd("No subagent sessions available")
	}
	return core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewCommandPaletteDialog([]commands.Category{{Name: "Subagents", Commands: items}})})
}
