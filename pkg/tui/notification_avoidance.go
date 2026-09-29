package tui

import (
	"image"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
)

func (m *appModel) syncNotificationAvoidance() {
	m.notification.SetRuntime(m.ar)
	// Update has already delivered Show/Hide messages by this point. Avoid
	// rendering and scanning the textarea when there is no overlay to move,
	// but clear old screen coordinates before a later notification opens.
	if !m.notification.Open() {
		m.notification.SetAvoidance(nil, false)
		return
	}
	active := m.width > 0 && m.height > 0 && m.editorTop() < m.height-m.messageBarHeight() && m.ready && m.err == nil && !m.contextClosed && !m.tickPaused && m.opening == nil && m.focusedPanel == PanelEditor && !m.dialogMgr.Open()
	var cells []image.Rectangle
	if occupancy, ok := m.editor.(editor.TextOccupancy); ok && active {
		frame := m.editorFrame()
		origin := image.Pt(frame.GetMarginLeft()+frame.GetBorderLeftSize()+frame.GetPaddingLeft(), m.editorTop()+frame.GetMarginTop()+frame.GetBorderTopSize()+frame.GetPaddingTop())
		for _, cell := range occupancy.OccupiedTextCells() {
			cells = append(cells, cell.Add(origin))
		}
	}
	m.notification.SetAvoidance(cells, active)
}
