package tui

import (
	"context"
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func (m *appModel) handleOpenAgentPicker() (tea.Model, tea.Cmd) {
	sess := m.application.Session()
	if sess == nil {
		return m, nil
	}
	return m, core.CmdHandler(dialog.OpenDialogMsg{Model: dialog.NewAgentPickerDialog(m.sessionState.AvailableAgents(), m.sessionState.CurrentAgentName(), sess.WorkingDir)})
}

func (m *appModel) handleNewAgentSession(msg messages.NewAgentSessionMsg) (tea.Model, tea.Cmd) {
	if cmd, ok := m.prepareReviewInputTransition(); !ok {
		return m, cmd
	}
	found := false
	for _, a := range m.sessionState.AvailableAgents() {
		if a.Name == msg.AgentName {
			found = true
			break
		}
	}
	if !found {
		return m, notification.ErrorCmd("Agent is no longer available: " + msg.AgentName)
	}
	if m.application.SessionRuntime() == nil {
		return m, notification.InfoCmd("Creating agent sessions is not supported for this session")
	}
	current := m.application.Session()
	if current == nil {
		return m, notification.ErrorCmd("No active session")
	}
	sess := session.New(session.WithAgentName(msg.AgentName), session.WithWorkingDir(msg.WorkingDir), session.WithToolsApproved(current.ToolsApproved), session.WithSafetyPolicy(current.GetSafetyPolicy()), session.WithHideToolResults(current.HideToolResults))
	// Create through the current view's owner so foreign and remote team bindings stay intact.
	createdID := ""
	err := m.supervisor.WithSessionOwner(m.ctx(), current.ID, func(ctx context.Context, resources supervisor.ViewOwnerResources) error {
		handle, err := resources.Sessions.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: msg.AgentName})
		if err == nil {
			createdID = handle.ID()
		}
		return err
	})
	if err != nil {
		return m, notification.ErrorCmd(fmt.Sprintf("Cannot start agent session: %v", err))
	}
	return m, m.beginHostedLoad(createdID, "", nil)
}
