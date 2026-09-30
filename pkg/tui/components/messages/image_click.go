package messages

import (
	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/image"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
)

type imageClick struct {
	preview *image.Preview
	x, y    int
}

func (m *model) SetImagePreviewSessionID(id string) {
	if id != m.imageSessionID {
		m.cancelImageClick()
		m.imageSessionID = id
	}
}
func (m *model) cancelImageClick() {
	if m.imageClick != nil {
		m.imageClick.preview.Close()
		m.imageClick = nil
	}
}
func (m *model) beginImageClick(msg tea.MouseClickMsg) bool {
	if msg.X < m.xPos || msg.X >= m.xPos+m.width || msg.Y < m.yPos || msg.Y >= m.yPos+m.height {
		return false
	}
	img, ok := image.AtPosition(m.View(), msg.X-m.xPos, msg.Y-m.yPos)
	if !ok {
		return false
	}
	preview := image.PreviewFromInline(img)
	if preview == nil {
		return false
	}
	m.imageClick = &imageClick{preview: preview, x: msg.X, y: msg.Y}
	line, col := m.mouseToLineCol(msg.X, msg.Y)
	m.selection.resetClickTracking()
	m.selection.detectClickType(line, col)
	m.selection.start(line, col)
	m.selection.mouseY = msg.Y
	return true
}
func (m *model) releaseImageClick(msg tea.MouseReleaseMsg) (tea.Cmd, bool) {
	candidate := m.imageClick
	if candidate == nil {
		return nil, false
	}
	m.imageClick = nil
	if msg.Button != tea.MouseLeft || msg.X != candidate.x || msg.Y != candidate.y {
		candidate.preview.Close()
		return nil, false
	}
	m.selection.clear()
	return core.CmdHandler(msgtypes.OpenImagePreviewMsg{SessionID: m.imageSessionID, Preview: candidate.preview}), true
}

// CancelImagePreviewClick abandons a press when another surface takes input.
func (m *model) CancelImagePreviewClick() { m.cancelImageClick() }
