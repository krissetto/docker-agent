package messages

import (
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/tool/subagenttool"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func (m *model) RefreshInputReferences() {
	for i, msg := range m.messages {
		// Tool renderers resolve names/colors from the shared index on render,
		// including tool children whose enclosing reasoning message has no ref.
		if msg.Type == types.MessageTypeToolCall || msg.Type == types.MessageTypeAssistantReasoningBlock {
			m.invalidateItem(i)
		}
		if lifecycle.IsUserInput(msg.InputOrigin) {
			continue
		}
		ref := m.resolveInputReference(msg)
		if ref == msg.InputReference {
			continue
		}
		m.CancelReferenceHover()
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
	if ref, ok := m.agentMessageReferenceForLine(index, localLine); ok {
		return ref, true
	}
	if msg.Type == types.MessageTypeAgentInput || msg.Type == types.MessageTypeRuntimeNotice {
		if view, ok := m.views[index].(interface{ InputReferenceOnLine(line int) bool }); ok && !view.InputReferenceOnLine(localLine) {
			return lifecycle.InputReference{}, false
		}
		return msg.InputReference, msg.InputReference.Kind != lifecycle.InputReferenceUnknown
	}
	if id, ok := subagenttool.NodeIDFor(msg); ok {
		if view, ok := m.views[index].(interface{ InputReferenceOnLine(line int) bool }); ok && !view.InputReferenceOnLine(localLine) {
			return lifecycle.InputReference{}, false
		}
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

// Old accepted reports can retain a stale child session ID. Only a runtime
// report's anchored header may recover an exact node still in the registry.
func (m *model) resolveInputReference(msg *types.Message) lifecycle.InputReference {
	ref := m.subagents.Resolve(m.inputParentSessionID, msg.SenderID, msg.SenderName)
	if ref.Kind != lifecycle.InputReferenceUnknown || msg.Type != types.MessageTypeRuntimeNotice {
		return ref
	}
	header, ok := strings.CutPrefix(msg.ReceivedBody, "Subagent ")
	if !ok {
		return ref
	}
	quoted, err := strconv.QuotedPrefix(header)
	if err != nil || !strings.HasPrefix(quoted, `"`) {
		return ref
	}
	if _, err := strconv.Unquote(quoted); err != nil {
		return ref
	}
	rest, ok := strings.CutPrefix(header[len(quoted):], " (")
	if !ok {
		return ref
	}
	id, rest, ok := strings.Cut(rest, ") ")
	if !ok || id == "" {
		return ref
	}
	verb := "finished its turn."
	if strings.HasPrefix(rest, "failed.") {
		verb = "failed."
	}
	tail, ok := strings.CutPrefix(rest, verb)
	if !ok || (tail != "" && !strings.HasPrefix(tail, " ")) {
		return ref
	}
	resolved := m.subagents.Resolve("", id, "")
	if resolved.Kind == lifecycle.InputReferenceNode && resolved.ID == id {
		return resolved
	}
	return ref
}

func (m *model) agentMessageReferenceForLine(index, line int) (lifecycle.InputReference, bool) {
	if index >= 0 && index < len(m.views) {
		if view, ok := m.views[index].(interface {
			InputReferenceForLine(line int) (lifecycle.InputReference, bool)
		}); ok {
			return view.InputReferenceForLine(line)
		}
	}
	return lifecycle.InputReference{}, false
}

func (m *model) AgentMessageIdentityAt(x, y int) (*types.Message, bool) {
	if _, ok := m.InputReferenceAt(x, y); !ok {
		return nil, false
	}
	line, _ := m.mouseToLineCol(x, y)
	index, local, _ := m.globalLineToMessageLine(line)
	_, ok := m.agentMessageReferenceForLine(index, local)
	if !ok {
		return nil, false
	}
	return m.messages[index], true
}
