package messages

import (
	"bytes"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type userAttachmentClick struct {
	message       *types.Message
	attachment    types.UserAttachment
	attachmentIdx int
	messageIdx    int
	x, y          int
}

func (m *model) beginUserAttachmentClick(msg tea.MouseClickMsg, messageIdx, line, col int) bool {
	if msg.Button != tea.MouseLeft || messageIdx < 0 || messageIdx >= len(m.messages) || m.messages[messageIdx].Type != types.MessageTypeUser {
		return false
	}
	view, ok := m.views[messageIdx].(interface {
		AttachmentAt(line, col int) (int, bool)
	})
	if !ok {
		return false
	}
	attachmentIdx, ok := view.AttachmentAt(line, col)
	if !ok || attachmentIdx < 0 || attachmentIdx >= len(m.messages[messageIdx].Attachments) {
		return false
	}
	attachment := m.messages[messageIdx].Attachments[attachmentIdx]
	attachment.Data = slices.Clone(attachment.Data)
	globalLine, globalCol := m.mouseToLineCol(msg.X, msg.Y)
	m.attachmentClick = &userAttachmentClick{
		message: m.messages[messageIdx], attachment: attachment, attachmentIdx: attachmentIdx,
		messageIdx: messageIdx, x: msg.X, y: msg.Y,
	}
	m.selection.resetClickTracking()
	m.selection.detectClickType(globalLine, globalCol)
	m.selection.start(globalLine, globalCol)
	m.selection.mouseY = msg.Y
	return true
}

func (m *model) releaseUserAttachmentClick(msg tea.MouseReleaseMsg) (tea.Cmd, bool) {
	candidate := m.attachmentClick
	if candidate == nil {
		return nil, false
	}
	m.attachmentClick = nil
	if msg.Button != tea.MouseLeft || msg.X != candidate.x || msg.Y != candidate.y ||
		candidate.messageIdx < 0 || candidate.messageIdx >= len(m.messages) || m.messages[candidate.messageIdx] != candidate.message {
		return nil, false
	}
	view, ok := m.views[candidate.messageIdx].(interface {
		AttachmentAt(line, col int) (int, bool)
	})
	if !ok {
		return nil, false
	}
	line, col := m.mouseToLineCol(msg.X, msg.Y)
	currentMessage, localLine := m.globalLineToMessageLineCached(line)
	if currentMessage != candidate.messageIdx {
		return nil, false
	}
	if block := m.workBlockForSource(currentMessage); block != nil {
		if childCol, inside := block.SourceColumnAt(line-m.workRangeStart(currentMessage), col); inside {
			col = childCol
		} else {
			return nil, false
		}
	}
	currentIdx, ok := view.AttachmentAt(localLine, col)
	if !ok || currentIdx != candidate.attachmentIdx || currentIdx >= len(candidate.message.Attachments) || !sameUserAttachment(candidate.attachment, candidate.message.Attachments[currentIdx]) {
		return nil, false
	}
	m.selection.clear()
	m.releaseRetainedReplies()
	attachment := candidate.attachment
	return core.CmdHandler(msgtypes.OpenUserAttachmentPreviewMsg{SessionID: m.imageSessionID, Attachment: attachment}), true
}

func sameUserAttachment(a, b types.UserAttachment) bool {
	return a.Name == b.Name && a.MediaType == b.MediaType && a.SourcePath == b.SourcePath &&
		a.Text == b.Text && a.Available == b.Available && a.UnavailableReason == b.UnavailableReason && bytes.Equal(a.Data, b.Data)
}
