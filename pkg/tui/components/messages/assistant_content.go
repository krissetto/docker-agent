package messages

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/streamcontent"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Identity-aware assistant content. Callers resolve a session and message
// identity first (see streamcontent.Tracker); these methods only keep a
// logical assistant message separate from its neighbours.

func (m *model) trackMessageIdentity(sessionID, messageID string) {
	for _, last := range slices.Backward(m.messages) {
		if last.Type == types.MessageTypeSpinner {
			continue
		}
		if identityChanged(last.SessionID, last.MessageID, sessionID, messageID) {
			m.BreakMessageGroup()
		}
		return
	}
}

// identityChanged reports whether a chunk with the incoming identity starts a
// new logical message after the last message, which has the given identity.
//
// A runtime message ID starts a new message when it differs. Chunks without a
// runtime ID (legacy or empty) keep today's behaviour: they join an unidentified
// message (a snapshot row, a standalone tool) and break only away from a
// message that carries a runtime ID or another session.
func identityChanged(lastSession, lastMessage, sessionID, messageID string) bool {
	runtimeID := messageID != "" && !streamcontent.IsLegacy(messageID)
	if lastSession == "" && lastMessage == "" {
		return runtimeID
	}
	if lastSession != sessionID {
		return true
	}
	if lastMessage == messageID {
		return false
	}
	if !runtimeID {
		return lastMessage != "" && !streamcontent.IsLegacy(lastMessage)
	}
	return true
}

// AppendAssistantContent keeps separate attempts and interleaved sessions distinct.
func (m *model) AppendAssistantContent(sessionID, messageID, agentName, content string) tea.Cmd {
	m.trackMessageIdentity(sessionID, messageID)
	cmd := m.AppendToLastMessage(agentName, content)
	if last := m.lastMessage(); last != nil {
		last.SessionID, last.MessageID = sessionID, messageID
	}
	return cmd
}

// AppendReasoningContent uses the same logical boundary as assistant content.
func (m *model) AppendReasoningContent(sessionID, messageID, agentName, content string) tea.Cmd {
	m.trackMessageIdentity(sessionID, messageID)
	cmd := m.AppendReasoning(agentName, content)
	if last := m.lastMessage(); last != nil {
		last.SessionID, last.MessageID = sessionID, messageID
	}
	return cmd
}

// ResetReasoningContent clears the reasoning of every matching block, including
// its copyable backing text. Tool entries and other messages are kept; a block
// left with no tools is removed.
func (m *model) ResetReasoningContent(sessionID, messageID, agentName string) tea.Cmd {
	materialize := m.materializeDeferredTail()
	for i, msg := range slices.Backward(m.messages) {
		if msg.Type != types.MessageTypeAssistantReasoningBlock || msg.SessionID != sessionID || msg.MessageID != messageID || msg.Sender != agentName {
			continue
		}
		block, ok := m.views[i].(*reasoningblock.Model)
		if !ok {
			continue
		}
		block.ClearReasoning()
		msg.Content = ""
		if block.ToolCount() == 0 {
			m.removeReasoningBlockAt(i, block)
		}
	}
	m.invalidateTranscriptStructure()
	return materialize
}

// removeReasoningBlockAt removes a block that no longer carries any content and
// keeps every index-based cursor in step with the shorter message list.
func (m *model) removeReasoningBlockAt(i int, block *reasoningblock.Model) {
	// Removing a message is a grouping change; the caller's epoch handles the views.
	m.workProjectionDirty = true
	animation.StopView(block)
	m.messages = slices.Delete(m.messages, i, i+1)
	m.indexMessages(i)
	m.views = slices.Delete(m.views, i, i+1)
	m.markSlotsStale()
	if i < m.groupStart {
		m.groupStart--
	}
	if i < m.loadedMessageCount {
		m.loadedMessageCount--
	}
	if i < m.committedMessageCount {
		m.committedMessageCount--
	}
	if m.selectedMessageIndex == i {
		m.selectedMessageIndex = -1
	} else if m.selectedMessageIndex > i {
		m.selectedMessageIndex--
	}
	if m.hoveredMessageIndex == i {
		m.hoveredMessageIndex = -1
	} else if m.hoveredMessageIndex > i {
		m.hoveredMessageIndex--
	}
	if m.streamedReasoning != nil && m.streamedReasoning.block == block {
		m.streamedReasoning = nil
	}
}

// ReconcileAssistantContent replaces incomplete streamed text with the saved answer.
func (m *model) ReconcileAssistantContent(sessionID, messageID, agentName, content string) tea.Cmd {
	if content == "" {
		return nil
	}
	materialize := m.materializeDeferredTail()
	var indices []int
	var streamed strings.Builder
	for i, msg := range m.messages {
		if msg.Type == types.MessageTypeAssistant && msg.SessionID == sessionID && msg.MessageID == messageID && msg.Sender == agentName {
			indices = append(indices, i)
			streamed.WriteString(msg.Content)
		}
	}
	content = strings.ReplaceAll(content, "\t", "    ")
	if len(indices) == 0 {
		return tea.Batch(materialize, m.AppendAssistantContent(sessionID, messageID, agentName, content))
	}
	if streamed.String() == content {
		return materialize
	}
	// Keep segment boundaries when the canonical text extends the delivered prefix.
	var cmds []tea.Cmd
	cmds = append(cmds, materialize)
	if strings.HasPrefix(content, streamed.String()) {
		i := indices[len(indices)-1]
		msg := m.messages[i]
		msg.Content += strings.TrimPrefix(content, streamed.String())
		cmds = append(cmds, m.views[i].(message.Model).SetMessage(msg))
		m.invalidateItem(i)
	} else {
		for n, i := range indices {
			msg := m.messages[i]
			msg.Content = ""
			if n == 0 {
				msg.Content = content
			}
			cmds = append(cmds, m.views[i].(message.Model).SetMessage(msg))
			m.invalidateItem(i)
		}
	}
	// Compact work projections cache the assistant text; rebuild them from the
	// settled content, as the streaming append path does.
	if m.workEnabled {
		m.workProjectionDirty = true
		m.layoutViewport()
	}
	m.updateReplyHandoffTarget()
	m.visualGeneration++
	return tea.Batch(cmds...)
}

// AppendAssistantMediaContent keeps media joined to its logical text message.
// Media that is already attached (same non-empty Key) is not added again, so a
// replayed canonical reference joins the item shown earlier.
func (m *model) AppendAssistantMediaContent(sessionID, messageID, agentName string, media []types.AssistantMedia) tea.Cmd {
	for i, msg := range slices.Backward(m.messages) {
		legacyMatch := msg.MessageID == "" && strings.HasPrefix(messageID, "legacy:") && slices.ContainsFunc(msg.AssistantMedia, func(existing types.AssistantMedia) bool {
			return existing.Key != "" && slices.ContainsFunc(media, func(item types.AssistantMedia) bool { return item.Key == existing.Key })
		})
		if msg.Type != types.MessageTypeAssistant || msg.SessionID != sessionID || (msg.MessageID != messageID && !legacyMatch) || msg.Sender != agentName {
			continue
		}
		var added []types.AssistantMedia
		for _, item := range media {
			if item.Key != "" && slices.ContainsFunc(msg.AssistantMedia, func(existing types.AssistantMedia) bool { return existing.Key == item.Key }) {
				continue
			}
			added = append(added, item)
		}
		if len(added) == 0 {
			return nil
		}
		msg.AssistantMedia = slices.Concat(msg.AssistantMedia, added)
		cmd := m.views[i].(message.Model).SetMessage(msg)
		m.invalidateItem(i)
		return cmd
	}
	m.trackMessageIdentity(sessionID, messageID)
	cmd := m.AppendAssistantMedia(agentName, media)
	if last := m.lastMessage(); last != nil {
		last.SessionID, last.MessageID = sessionID, messageID
	}
	return cmd
}

// AdoptAssistantMediaIdentity joins legacy canonical replay to restored manifest media.
func (m *model) AdoptAssistantMediaIdentity(sessionID, messageID, agentName string, media []types.AssistantMedia) {
	if !strings.HasPrefix(messageID, "legacy:") {
		return
	}
	for _, msg := range m.messages {
		if msg.Type != types.MessageTypeAssistant || msg.SessionID != sessionID || msg.MessageID != "" || msg.Sender != agentName {
			continue
		}
		if slices.ContainsFunc(msg.AssistantMedia, func(existing types.AssistantMedia) bool {
			return existing.Key != "" && slices.ContainsFunc(media, func(item types.AssistantMedia) bool { return item.Key == existing.Key })
		}) {
			msg.MessageID = messageID
			return
		}
	}
}
