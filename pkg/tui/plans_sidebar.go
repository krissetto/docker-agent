package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

// planSidebarEnabled reports whether the opt-in Plans section is visible. The
// lean and hidden-sidebar layouts never load plan rows for it.
func (m *appModel) planSidebarEnabled() bool {
	return m.layoutSettings.ShowPlans && !m.leanMode && !m.hideSidebar
}

// planDataVisible reports whether plan rows are on screen: the sidebar section
// or an open /plans dialog. Live refreshes run only while this holds.
func (m *appModel) planDataVisible() bool {
	return m.planSidebarEnabled() || m.planDialogOpen()
}

func (m *appModel) refreshPlanSidebarCmd() tea.Cmd {
	if !m.planSidebarEnabled() {
		return nil
	}
	return m.planRefreshCmd(false)
}

// updatePlanSidebar shares one app-owned plan snapshot with every session page,
// so a background tab's plan change is already current when it becomes active.
func (m *appModel) updatePlanSidebar(data messages.PlanSidebarDataMsg) tea.Cmd {
	m.planSidebarData = data
	var cmds []tea.Cmd
	for _, page := range m.chatPages {
		if page == nil {
			continue
		}
		_, cmd := page.Update(data)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

func (m *appModel) cancelSidebarPlanEdit() {
	m.sidebarPlanEditGeneration++
	m.sidebarPlanEditInFlight = false
}

func (m *appModel) handleEditSidebarPlan(msg messages.EditSidebarPlanMsg) (tea.Model, tea.Cmd) {
	if msg.TabID != "" && (m.supervisor == nil || m.supervisor.ActiveID() != msg.TabID) {
		return m, nil
	}
	if !m.planSidebarEnabled() || m.dialogMgr.Open() || m.sidebarPlanEditInFlight {
		return m, nil
	}
	m.sidebarPlanEditGeneration++
	m.sidebarPlanEditInFlight = true
	cmd := m.preparePlanEdit(messages.EditPlanMsg{
		Ref: msg.Ref, ExpectedVersion: msg.ExpectedVersion,
	}, m.sidebarPlanEditGeneration)
	return m, cmd
}
