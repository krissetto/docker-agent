package tui

import (
	tea "charm.land/bubbletea/v2"

	subagentpkg "github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Tabs align with the editor frame, not the inset editable text.
func tabFrameOrigin() int {
	return styles.EditorStyle.GetMarginLeft()
}

func tabFrameWidth(width int) int {
	return max(0, width-styles.EditorStyle.GetHorizontalMargins())
}

// tabAgentIdentity reads the selected agent, never the session title or a guessed ID.
func (m *appModel) tabAgentIdentity(tab messages.TabInfo) (name, nodeID string) {
	name, nodeID = tab.AgentName, tab.AgentNodeID
	if m.supervisor == nil {
		return name, nodeID
	}
	runner := m.supervisor.GetRunner(tab.SessionID)
	if runner == nil || runner.App == nil {
		return name, nodeID
	}
	application := runner.App
	name = application.Binding().AgentName
	if state := m.sessionStates[tab.SessionID]; state != nil && state.CurrentAgentName() != "" {
		name = state.CurrentAgentName()
	}
	if attached := application.AttachedSubagent(); attached != nil {
		if name == "" {
			name = attached.Agent
		}
		return name, string(attached.NodeID)
	}
	if sess := application.Session(); sess != nil {
		if name == "" {
			name = sess.AgentName
		}
		if lookup, ok := application.Runtime().(subagentSessionLookup); ok {
			if node, found := lookup.SubagentNodeForSession(sess.ID); found {
				return name, string(node)
			}
		}
		nodeID = string(subagentpkg.SessionRootID(sess.ID))
	}
	return name, nodeID
}

func (m *appModel) setTabs(tabs []messages.TabInfo, activeIdx int) tea.Cmd {
	m.tabInfos = append(m.tabInfos[:0], tabs...)
	for i := range m.tabInfos {
		m.tabInfos[i].IsActive = i == activeIdx
	}
	for i := range m.tabInfos {
		m.tabInfos[i].AgentName, m.tabInfos[i].AgentNodeID = m.tabAgentIdentity(m.tabInfos[i])
	}
	return m.tabBar.SetTabs(m.tabInfos, activeIdx)
}

func (m *appModel) syncTabAgents() tea.Cmd {
	if m.tabBar == nil {
		return nil
	}
	changed, activeIdx := false, 0
	for i := range m.tabInfos {
		tab := &m.tabInfos[i]
		if tab.IsActive {
			activeIdx = i
		}
		name, nodeID := m.tabAgentIdentity(*tab)
		if tab.AgentName != name || tab.AgentNodeID != nodeID {
			tab.AgentName, tab.AgentNodeID = name, nodeID
			changed = true
		}
	}
	if !changed {
		return nil
	}
	m.viewCacheValid = false
	return m.tabBar.SetTabs(m.tabInfos, activeIdx)
}
