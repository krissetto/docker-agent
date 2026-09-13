package messages

import (
	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/tool/subagenttool"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func (m *model) RefreshInputReferences() {
	for i, msg := range m.messages {
		if lifecycle.IsUserInput(msg.InputOrigin) {
			continue
		}
		ref := m.subagents.Resolve(m.inputParentSessionID, msg.SenderID, msg.SenderName)
		if ref == msg.InputReference {
			continue
		}
		msg.InputReference = ref
		if view, ok := m.views[i].(message.Model); ok {
			view.InvalidateRenderCache()
		}
		m.invalidateItem(i)
	}
}

func (m *model) referenceForMessage(index, localLine int) (lifecycle.InputReference, bool) {
	if index < 0 || index >= len(m.messages) {
		return lifecycle.InputReference{}, false
	}
	msg := m.messages[index]
	if msg.Type == types.MessageTypeAgentInput || msg.Type == types.MessageTypeRuntimeNotice {
		if msg.Type == types.MessageTypeAgentInput && localLine != 0 {
			return lifecycle.InputReference{}, false
		}
		return msg.InputReference, msg.InputReference.Kind != lifecycle.InputReferenceUnknown
	}
	if id, ok := subagenttool.NodeIDFor(msg); ok {
		ref := m.subagents.Resolve("", string(id), "")
		// The tool's stamped node ID is already a canonical node reference.
		ref.Kind, ref.ID = lifecycle.InputReferenceNode, string(id)
		return ref, true
	}
	return lifecycle.InputReference{}, false
}

func (m *model) InputReferenceAt(x, y int) (lifecycle.InputReference, bool) {
	if x < m.xPos || x >= m.xPos+m.contentWidth() || y < m.yPos || y >= m.yPos+m.height {
		return lifecycle.InputReference{}, false
	}
	line, col := m.mouseToLineCol(x, y)
	index, local, _ := m.globalLineToMessageLine(line)
	ref, ok := m.referenceForMessage(index, local)
	if !ok {
		return ref, false
	}
	for _, span := range m.urlSpans.get(line, m.renderedLine(line)) {
		if span.url == agentidentity.Link && col >= span.startCol && col < span.endCol {
			return ref, true
		}
	}
	return ref, false
}
