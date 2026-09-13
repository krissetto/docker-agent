package tui

import (
	"errors"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type pendingEditResult struct {
	editorID          uint64
	sessionID, turnID string
	err               error
}

func (m *appModel) openPendingMessageEdit(msg messages.OpenPendingEditMsg) (tea.Model, tea.Cmd) {
	if m.application == nil || m.application.Session() == nil || m.application.Session().ID != msg.SessionID || msg.TurnID == "" || m.dialogMgr.Open() {
		return m, nil
	}
	m.pendingEditorGeneration++
	editorID := m.pendingEditorGeneration
	origin, handle, ctx := m.application, m.application.SessionHandle(), m.ctx()
	save := func(content string) tea.Cmd {
		if m.application != origin || origin.Session() == nil || origin.Session().ID != msg.SessionID || origin.SessionHandle() != handle {
			return core.CmdHandler(dialog.PendingMessageSaveResultMsg{EditorID: editorID, SessionID: msg.SessionID, TurnID: msg.TurnID, Err: errors.New("the session changed; the queued message was not edited")})
		}
		return func() tea.Msg {
			err := origin.EditPendingMessage(ctx, msg.SessionID, msg.TurnID, content)
			return pendingEditResult{editorID: editorID, sessionID: msg.SessionID, turnID: msg.TurnID, err: err}
		}
	}
	editor := dialog.NewPendingMessageEditDialog(msg.SessionID, msg.TurnID, msg.Content, editorID, save)
	return m.forwardDialog(dialog.OpenDialogMsg{Model: editor})
}

func (m *appModel) handlePendingEditResult(result pendingEditResult) (tea.Model, tea.Cmd) {
	matched := m.dialogMgr.HasDialog(func(d dialog.Dialog) bool {
		editor, ok := d.(interface{ PendingEditID() uint64 })
		return ok && editor.PendingEditID() == result.editorID
	})
	if !matched {
		return m, nil
	}
	// Instance IDs, not the current top layer, scope results while another modal covers this draft.
	return m.forwardDialog(dialog.PendingMessageSaveResultMsg{EditorID: result.editorID, SessionID: result.sessionID, TurnID: result.turnID, Err: result.err})
}
