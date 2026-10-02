package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func applySavedSubagentsPreference(a *app.App) {
	if a != nil {
		if rt, ok := a.Runtime().(runtime.SubagentPolicy); ok {
			rt.SetUseSubagents(userconfig.Get().GetUseSubagents())
		}
	}
}

func subagentsPreference(a *app.App) bool {
	if a != nil {
		if rt, ok := a.Runtime().(runtime.SubagentPolicy); ok {
			return rt.UseSubagents()
		}
	}
	return userconfig.Get().GetUseSubagents()
}

func (m *appModel) setUseSubagents(enabled bool) tea.Cmd {
	if m.application == nil {
		return notification.ErrorCmd("No active runtime; preference not saved")
	}
	if m.supervisor != nil {
		if err := m.supervisor.SaveUseSubagents(enabled, m.application.Runtime(), userconfig.SetUseSubagents); err != nil {
			return notification.ErrorCmd(err.Error())
		}
	} else {
		rt, ok := m.application.Runtime().(runtime.SubagentPolicy)
		if !ok {
			return notification.ErrorCmd("Use subagents is unavailable on this runtime; preference not saved")
		}
		if err := userconfig.SetUseSubagents(enabled); err != nil {
			return notification.ErrorCmd("Failed to save Use subagents: " + err.Error())
		}
		rt.SetUseSubagents(enabled)
	}
	var refresh tea.Cmd
	for _, data := range m.panelData {
		if data.treeDialog != nil {
			msg := dialog.SubagentsPolicyMsg{Enabled: enabled}
			if m.dialogMgr != nil && m.dialogMgr.TopDialog() == data.treeDialog {
				refresh = tea.Batch(refresh, m.updateDialogCmd(msg))
			} else {
				data.treeDialog.Update(msg)
			}
		}
	}
	m.viewCacheValid = false
	label := "OFF"
	if enabled {
		label = "ON"
	}
	return tea.Batch(refresh, notification.SuccessCmd("Use subagents: "+label+" (saved globally for local sessions; new delegation only, existing work continues)"))
}

func (m *appModel) stopSubagentSubtree(id string) tea.Cmd {
	if m.application == nil {
		return notification.ErrorCmd("No active runtime")
	}
	control, ok := m.application.Runtime().(runtime.SubagentControl)
	if !ok {
		return notification.ErrorCmd("Stop subtree is unavailable on this runtime")
	}
	return func() tea.Msg {
		if err := control.StopSubtree(subagent.NodeID(id)); err != nil {
			return notification.ErrorCmd("Stop subtree: " + err.Error())()
		}
		return notification.SuccessCmd("Subtree stopped permanently; transcript remains available")()
	}
}
