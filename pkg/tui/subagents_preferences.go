package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func applySavedSubagentsPreference(a *app.App) {
	if a != nil {
		if rt, ok := a.Runtime().(*runtime.LocalRuntime); ok {
			rt.SetUseSubagents(userconfig.Get().GetUseSubagents())
		}
	}
}

func subagentsPreference(a *app.App) bool {
	if a != nil {
		if rt, ok := a.Runtime().(*runtime.LocalRuntime); ok {
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
		rt, ok := m.application.Runtime().(*runtime.LocalRuntime)
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
	return tea.Batch(refresh, notification.SuccessCmd("Use subagents: "+label))
}
