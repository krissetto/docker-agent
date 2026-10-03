package leantui

import (
	"context"
	"strings"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func (m *model) openSubagentPicker(ctx context.Context) {
	snapshot := m.subagentSnapshot
	if snapshot == nil {
		snapshot = m.app.SubagentTreeSnapshot()
	}
	if snapshot == nil {
		snapshot = &subagent.Snapshot{}
	}
	var attached subagent.NodeID
	if info := m.app.AttachedSubagent(); info != nil {
		attached = info.NodeID
	}
	m.screen.Autocomplete.Dismiss()
	picker := ui.NewSubagentPicker(*snapshot, m.app.Session().ID, attached)
	m.screen.Subagents = picker
	canonical := m.app.CanSetDelegationPolicy()
	_, localDefault := m.app.Runtime().(runtime.SubagentPolicy)
	picker.PolicyUnavailable, picker.StopUnavailable = !canonical && !localDefault, !m.app.CanStopSubtree()
	if !canonical {
		picker.UseSubagents = subagentsPreference(m.app)
		return
	}
	picker.PolicySessionTree, picker.PolicyPending = true, true
	application := m.app
	m.capabilityJob(ctx, func(jobCtx context.Context) (any, error) {
		enabled, err := application.DelegationPolicy(jobCtx)
		return delegationPolicyResult{enabled: enabled}, err
	})
}

// delegationPolicyResult is a canonical session-tree policy read or change.
type delegationPolicyResult struct{ enabled, set bool }

func (m *model) applyDelegationPolicy(result delegationPolicyResult, err error) {
	picker := m.screen.Subagents
	if picker != nil && picker.PolicySessionTree {
		picker.PolicyPending = false
		if err == nil {
			picker.UseSubagents = result.enabled
		} else if !result.set {
			picker.PolicyUnavailable = true
		}
	}
	if err != nil {
		m.reportCapability("Use subagents: "+err.Error(), nil)
		return
	}
	if result.set {
		m.reportCapability("Use subagents for this session tree: "+onOff(result.enabled)+" (new delegation only; existing work continues)", nil)
	}
}

func onOff(enabled bool) string {
	if enabled {
		return "ON"
	}
	return "OFF"
}

func (m *model) handleSubagentPickerKey(ctx context.Context, key ui.Key) {
	picker := m.screen.Subagents
	if target, handled := picker.HandleStopKey(key); handled {
		if target != "" {
			if !m.app.CanStopSubtree() {
				m.reportCapability("Stop subtree is unavailable on this runtime", nil)
				return
			}
			resolved, ok := m.app.ResolveSubagentTarget(string(target))
			if !ok && m.subagentSnapshot != nil {
				resolved, ok = app.FindSubagentTarget(*m.subagentSnapshot, string(target))
			}
			if !ok {
				resolved = app.SubagentTarget{NodeID: target}
			}
			application := m.app
			m.capabilityJob(ctx, func(jobCtx context.Context) (any, error) {
				return "Subtree stopped and drained; transcripts remain available", application.StopSubtree(jobCtx, resolved)
			})
		}
		return
	}
	if picker.HandleActionKey(key) {
		m.setUseSubagents(ctx, !picker.UseSubagents)
		return
	}
	switch key.Typ {
	case ui.KeyEsc:
		m.screen.Subagents = nil
	case ui.KeyUp, ui.KeyDown, ui.KeyLeft, ui.KeyRight, ui.KeyHome, ui.KeyEnd:
		m.screen.Subagents.Navigate(key.Typ)
	case ui.KeyEnter:
		if picker.PolicyActionFocused() {
			return
		}
		node, ok := m.screen.Subagents.Current()
		if !ok {
			return
		}
		m.screen.Subagents = nil
		if node.SessionID == m.app.Session().ID {
			return
		}
		// The tree root is context too: return to an already-open ancestor
		// without asking the runtime to treat it as a child attachment.
		if m.viewers != nil && node.SessionID != "" {
			for application, target := range m.viewers.views {
				if application.Session().ID == node.SessionID {
					m.focusViewer(target, true)
					return
				}
			}
		}
		m.attachSubagentViewer(ctx, string(node.ID))
	}
}

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

func (m *model) handleSubagentsCommand(ctx context.Context, arg string) {
	switch strings.TrimSpace(arg) {
	case "":
		m.openSubagentPicker(ctx)
	case "on":
		m.setUseSubagents(ctx, true)
	case "off":
		m.setUseSubagents(ctx, false)
	default:
		m.reportCapability("Usage: /subagents [on|off]", nil)
	}
}

// setUseSubagents changes the canonical session-tree policy when the owner
// holds one; only owners without it fall back to the saved local default.
func (m *model) setUseSubagents(ctx context.Context, enabled bool) {
	if application := m.app; application.CanSetDelegationPolicy() {
		if picker := m.screen.Subagents; picker != nil {
			picker.PolicyPending = true
		}
		m.capabilityJob(ctx, func(jobCtx context.Context) (any, error) {
			return delegationPolicyResult{enabled: enabled, set: true}, application.SetDelegationPolicy(jobCtx, enabled)
		})
		return
	}
	var runtimes []runtime.SubagentPolicy
	add := func(a *app.App) bool {
		if a == nil {
			return false
		}
		rt, ok := a.Runtime().(runtime.SubagentPolicy)
		if ok {
			runtimes = append(runtimes, rt)
		}
		return ok
	}
	if !add(m.app) {
		m.reportCapability("Use subagents is unavailable on this runtime; preference not saved", nil)
		return
	}
	if m.viewers != nil {
		for a := range m.viewers.views {
			if !add(a) {
				m.reportCapability("Use subagents cannot be applied to a viewer without policy support; preference not saved", nil)
				return
			}
		}
	}
	save := userconfig.SetUseSubagents
	if owner, ok := m.sessionViews.(interface {
		SaveUseSubagents(bool, app.Services, func(bool) error) error
	}); ok {
		save = func(value bool) error {
			return owner.SaveUseSubagents(value, m.app.Runtime(), userconfig.SetUseSubagents)
		}
	}
	if err := save(enabled); err != nil {
		m.reportCapability("Failed to save Use subagents: "+err.Error(), nil)
		return
	}
	for _, rt := range runtimes {
		rt.SetUseSubagents(enabled)
	}
	if m.screen.Subagents != nil && !m.screen.Subagents.PolicySessionTree {
		m.screen.Subagents.UseSubagents = enabled
	}
	if m.viewers != nil {
		for _, view := range m.viewers.views {
			if view.screen.Subagents != nil && !view.screen.Subagents.PolicySessionTree {
				view.screen.Subagents.UseSubagents = enabled
			}
		}
	}
	m.reportCapability("Use subagents local default: "+onOff(enabled)+" (saved for new local sessions; new delegation only, existing work continues)", nil)
}

func (m *model) syncSubagentsCompletion() {
	text := m.screen.Editor.Text()
	if text == m.subagentsCompletionText {
		return
	}
	m.subagentsCompletionText = text
	if strings.HasPrefix(text, "/subagents ") {
		var choices []ui.Command
		for _, name := range []string{"on", "off"} {
			choices = append(choices, ui.Command{
				Name: name, Desc: "Use subagents: " + name, Kind: ui.CmdBuiltin,
				MatchScore: func(query string) (int, bool) { return 0, strings.HasPrefix(name, query) },
			})
		}
		m.screen.Autocomplete.SetScopedCommands("subagents ", choices)
	}
}
