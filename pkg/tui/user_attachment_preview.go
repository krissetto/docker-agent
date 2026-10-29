package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func (m *appModel) openUserAttachmentPreview(msg messages.OpenUserAttachmentPreviewMsg) (tea.Model, tea.Cmd) {
	if msg.SessionID == "" || m.supervisor == nil || m.supervisor.FindBySession(msg.SessionID) == nil || m.dialogMgr.Open() {
		return m, nil
	}
	supported := m.imageWriter != nil && m.imageWriter.Supported()
	preview := dialog.NewUserAttachmentPreviewDialog(m.ar, msg.Attachment, supported)
	if image, ok := preview.(interface {
		Update(msg tea.Msg) (layout.Model, tea.Cmd)
	}); ok {
		image.Update(dialog.ImageCellSizeMsg(m.imageCellSize))
	}
	return m.forwardDialog(dialog.OpenDialogMsg{Model: preview})
}
