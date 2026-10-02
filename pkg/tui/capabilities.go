package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// capabilityScope fences delayed metadata and dialog results by both bound App
// identity and route generation. A tab switch must never paste into another tab.
type capabilityScope struct {
	application      *app.App
	sessionID, owner string
	generation       uint64
	epoch            uint64
}
type capabilityResult struct {
	metadataErr        error
	metadataGeneration uint64
	scope              capabilityScope
	message            tea.Msg
	refresh            bool
	branch             *session.Session
	target             string
	send               *messages.SendMsg
}

func (m *appModel) captureCapabilityScope() capabilityScope {
	scope := capabilityScope{application: m.application, owner: m.paneFocus(), epoch: m.modelPickerGeneration}
	if m.application != nil && m.application.Session() != nil {
		scope.sessionID = m.application.Session().ID
	}
	if m.supervisor != nil {
		scope.generation, _ = m.supervisor.RouteGeneration(scope.owner)
	}
	return scope
}
func (m *appModel) capabilityScopeCurrent(scope capabilityScope) bool {
	if m.modelPickerGeneration != scope.epoch || m.contextClosed || m.application != scope.application || m.application == nil || m.application.Session() == nil || m.application.Session().ID != scope.sessionID || m.paneFocus() != scope.owner {
		return false
	}
	generation, _ := m.supervisor.RouteGeneration(scope.owner)
	return generation == scope.generation
}
func (m *appModel) capabilityCommand(work func(context.Context, *app.App) tea.Msg) tea.Cmd {
	scope, ctx := m.captureCapabilityScope(), m.ctx()
	bound := scope.application.CapabilityView()
	return func() tea.Msg { return capabilityResult{scope: scope, message: work(ctx, bound)} }
}
func (m *appModel) processCapabilityMessage(msg tea.Msg) tea.Cmd {
	// Process the payload in this same update, rather than enqueue an unscoped
	// follow-up which could land after a tab switch.
	_, cmd := m.update(msg)
	return cmd
}
func (m *appModel) prepareCommandMetadata() tea.Cmd {
	if m.contextClosed || m.application == nil || m.application.Session() == nil {
		return nil
	}
	id := m.application.Session().ID
	if m.metadataApp == m.application && m.metadataSessionID == id {
		return nil
	}
	m.metadataApp, m.metadataSessionID = m.application, id
	m.metadataGeneration++
	generation := m.metadataGeneration
	scope, ctx := m.captureCapabilityScope(), m.ctx()
	return func() tea.Msg {
		err := scope.application.RefreshCommandMetadata(ctx)
		return capabilityResult{scope: scope, refresh: true, metadataErr: err, metadataGeneration: generation}
	}
}
func (m *appModel) beginBranch(options runtime.BranchOptions, target string, send *messages.SendMsg) tea.Cmd {
	scope, ctx := m.captureCapabilityScope(), m.ctx()
	handle, sessions := scope.application.SessionHandle(), scope.application.SessionRuntime()
	return func() tea.Msg {
		var branched *session.Session
		err := runtime.UnsupportedSessionOperation(scope.sessionID, "branch")
		if brancher, ok := sessions.(runtime.SessionBrancher); ok && handle != nil && handle.Metadata().Capabilities.Branching {
			_, branched, err = brancher.BranchSession(ctx, scope.sessionID, options)
		}
		result := capabilityResult{scope: scope, branch: branched, target: target, send: send}
		if err != nil {
			result.branch = nil
			result.message = notification.ShowMsg{Text: "Cannot branch session; refresh the conversation if it changed: " + err.Error(), Type: notification.TypeError}
		}
		return result
	}
}
