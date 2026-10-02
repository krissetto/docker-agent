package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type pendingRemovalConfirmedMsg struct {
	origin            *app.App
	handle            runtime.SessionHandle
	sessionID, turnID string
	generation        uint64
}

func (m *appModel) openPendingMessageRemoval(msg messages.OpenPendingRemovalMsg) (tea.Model, tea.Cmd) {
	if m.application == nil || m.application.Session() == nil || m.application.Session().ID != msg.SessionID || msg.TurnID == "" || m.dialogMgr.Open() {
		return m, nil
	}
	request := pendingRemovalConfirmedMsg{origin: m.application, handle: m.application.SessionHandle(), sessionID: msg.SessionID, turnID: msg.TurnID, generation: m.pendingRemovalGeneration}
	return m.forwardDialog(dialog.OpenDialogMsg{Model: dialog.NewPendingMessageRemovalDialog(msg.Content, core.CmdHandler(request))})
}

func (m *appModel) confirmPendingMessageRemoval(msg pendingRemovalConfirmedMsg) (tea.Model, tea.Cmd) {
	if m.contextClosed || m.pendingRemovalGeneration != msg.generation || m.application != msg.origin || m.application == nil || m.application.Session() == nil || m.application.Session().ID != msg.sessionID || m.application.SessionHandle() != msg.handle || msg.handle == nil || msg.handle.ID() != msg.sessionID {
		return m, notification.InfoCmd("The session changed; the queued message was not removed")
	}
	canceler, ok := msg.handle.(runtime.PendingMessageCanceler)
	if !ok {
		return m, notification.ErrorCmd("Removing queued messages is not supported by this session")
	}
	ctx := m.ctx()
	// Capture the owner, not the mutable App route. The runtime linearizes this
	// exact TurnID's withdrawal with promotion under its existing owner lock.
	return m, func() tea.Msg {
		removed, err := canceler.CancelPendingMessage(ctx, msg.turnID)
		if err != nil {
			return notification.ShowMsg{Text: "Failed to remove queued message: " + err.Error(), Type: notification.TypeError}
		}
		if !removed {
			return notification.ShowMsg{Text: "Queued message is no longer pending", Type: notification.TypeInfo}
		}
		return nil
	}
}
