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

// delegationPolicyMsg carries a canonical session-tree policy read or change.
type delegationPolicyMsg struct {
	application *app.App
	enabled     bool
	set         bool
	err         error
}

// queryDelegationPolicy reads the effective tree policy off the event loop.
func (m *appModel) queryDelegationPolicy(application *app.App) tea.Cmd {
	if application == nil || !application.CanSetDelegationPolicy() {
		return nil
	}
	ctx := m.ctx()
	return func() tea.Msg {
		enabled, err := application.DelegationPolicy(ctx)
		return delegationPolicyMsg{application: application, enabled: enabled, err: err}
	}
}

func (m *appModel) applyDelegationPolicy(msg delegationPolicyMsg) tea.Cmd {
	var cmds []tea.Cmd
	if msg.err != nil {
		cmds = append(cmds, notification.ErrorCmd("Use subagents: "+msg.err.Error()))
		if msg.set {
			// Reread so the control never shows an unapplied change.
			cmds = append(cmds, m.queryDelegationPolicy(msg.application))
			return tea.Batch(cmds...)
		}
	}
	for _, data := range m.panelData {
		if data.treeDialog == nil || data.application != msg.application {
			continue
		}
		policy := dialog.SubagentsPolicyMsg{Enabled: msg.enabled, SessionTree: true}
		if msg.err != nil {
			data.treeDialog.Update(dialog.SubagentsCapabilitiesMsg{Policy: false, Stop: msg.application.CanStopSubtree()})
		}
		if m.dialogMgr != nil && m.dialogMgr.TopDialog() == data.treeDialog {
			cmds = append(cmds, m.updateDialogCmd(policy))
		} else {
			data.treeDialog.Update(policy)
		}
	}
	m.viewCacheValid = false
	if msg.set && msg.err == nil {
		cmds = append(cmds, notification.SuccessCmd("Use subagents for this session tree: "+onOff(msg.enabled)+" (new delegation only; existing work continues)"))
	}
	return tea.Batch(cmds...)
}

func onOff(enabled bool) string {
	if enabled {
		return "ON"
	}
	return "OFF"
}

// setUseSubagents changes the canonical session-tree policy when the owner
// holds one; only owners without it fall back to the saved local default.
func (m *appModel) setUseSubagents(enabled bool) tea.Cmd {
	if m.application == nil {
		return notification.ErrorCmd("No active runtime; preference not saved")
	}
	if application := m.application; application.CanSetDelegationPolicy() {
		ctx := m.ctx()
		return func() tea.Msg {
			return delegationPolicyMsg{application: application, enabled: enabled, set: true, err: application.SetDelegationPolicy(ctx, enabled)}
		}
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
		if data.treeDialog != nil && !data.application.CanSetDelegationPolicy() {
			msg := dialog.SubagentsPolicyMsg{Enabled: enabled}
			if m.dialogMgr != nil && m.dialogMgr.TopDialog() == data.treeDialog {
				refresh = tea.Batch(refresh, m.updateDialogCmd(msg))
			} else {
				data.treeDialog.Update(msg)
			}
		}
	}
	m.viewCacheValid = false
	return tea.Batch(refresh, notification.SuccessCmd("Use subagents local default: "+onOff(enabled)+" (saved for new local sessions; new delegation only, existing work continues)"))
}

func (m *appModel) stopSubagentSubtree(id string) tea.Cmd {
	application := m.application
	if application == nil {
		return notification.ErrorCmd("No active runtime")
	}
	if !application.CanStopSubtree() {
		return notification.ErrorCmd("Stop subtree is unavailable on this runtime")
	}
	target, ok := application.ResolveSubagentTarget(id)
	if data := m.panelData[m.paneFocus()]; !ok && data != nil {
		target, ok = app.FindSubagentTarget(subagent.Snapshot{Nodes: data.treeNodes}, id)
	}
	if !ok {
		return notification.ErrorCmd("Stop subtree: this subagent has no session")
	}
	ctx := m.ctx()
	return func() tea.Msg {
		if err := application.StopSubtree(ctx, target); err != nil {
			return notification.ErrorCmd("Stop subtree: " + err.Error())()
		}
		return notification.SuccessCmd("Subtree stopped and drained; transcripts remain available")()
	}
}
