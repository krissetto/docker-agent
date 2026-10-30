package tui

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

// planSidebarEnabled reports whether the opt-in Plans section is visible. The
// lean and hidden-sidebar layouts never load plan rows for it.
func (m *appModel) planSidebarEnabled() bool {
	return m.layoutSettings.ShowPlans && !m.leanMode && !m.hideSidebar
}

// planPanelEnabled reports whether the bottom panel's Plans element is
// configured. Lean mode hides the panel's plan data as it hides the section.
func (m *appModel) planPanelEnabled() bool {
	return !m.leanMode && slices.Contains(messages.NormalizePanelSettings(m.panelSettings).Elements, messages.PanelPlans)
}

// planDataShown reports whether shared plan rows are on screen outside a
// dialog: the sidebar section or the bottom panel element. The hidden sidebar
// does not hide the panel element.
func (m *appModel) planDataShown() bool {
	return m.planSidebarEnabled() || m.planPanelEnabled()
}

// planDataVisible reports whether plan rows are on screen: a shown surface or
// an open /plans dialog. Live refreshes run only while this holds.
func (m *appModel) planDataVisible() bool {
	return m.planDataShown() || m.planDialogOpen()
}

func (m *appModel) refreshPlanSidebarCmd() tea.Cmd {
	if !m.planDataShown() {
		return nil
	}
	return m.planRefreshCmd(false)
}

// planPanelLabel is the bottom panel's Plans element: the same snapshot the
// sidebar section shows, reduced to a count.
func (m *appModel) planPanelLabel() string {
	data := m.planSidebarData
	switch {
	case data.Err != nil:
		return "Plans unavailable"
	case data.Loading && len(data.Result.Plans) == 0:
		return "Loading plans"
	}
	if n := len(data.Result.Plans); n != 1 {
		return fmt.Sprintf("%d plans", n)
	}
	return "1 plan"
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
