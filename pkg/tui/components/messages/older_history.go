package messages

import (
	"maps"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// OlderHistoryRequestedMsg is emitted only on upward navigation near the loaded history edge.
type OlderHistoryRequestedMsg struct{}

func SetOlderHistoryAvailable(list Model, available bool) {
	if m, ok := list.(*model); ok {
		m.olderHistoryAvailable = available
	}
}

func AddHistoryToolResults(dst map[string]string, items []session.Item) {
	for id, result := range replayToolResults(items) {
		if _, newer := dst[id]; !newer {
			dst[id] = result
		}
	}
}

func MatchesUserMessage(list Model, index, position int, content string) bool {
	m := list.(*model)
	if index < 0 || index >= len(m.messages) {
		return false
	}
	msg := m.messages[index]
	return msg.Type == types.MessageTypeUser && msg.SessionPosition != nil && *msg.SessionPosition == position && msg.Content == content
}

func (m *model) olderHistoryRequest() tea.Cmd {
	return m.olderHistoryRequestAfterWheel(0)
}

func (m *model) olderHistoryRequestAfterWheel(delta int) tea.Cmd {
	if !m.olderHistoryAvailable || m.historyStart > 0 || m.scrollOffset+delta*wheelScrollAmount*defaultScrollAmount > max(2, m.bodyHeight()/3) {
		return nil
	}
	return func() tea.Msg { return OlderHistoryRequestedMsg{} }
}

// PrependHistory inserts already anchored historical items without replaying or
// replacing the live suffix. The replay renderer resolves legacy tool results
// across page boundaries, including live results for older calls.
func PrependHistory(list Model, prefix *session.Session, start int, newerResults map[string]string, media map[int][]types.AssistantMedia) tea.Cmd {
	m := list.(*model)
	m.updateScrollState()
	anchors := m.olderHistoryAnchors()
	toolResults := replayToolResults(prefix.Messages)
	for _, item := range prefix.Messages {
		if !item.IsMessage() {
			continue
		}
		for _, call := range item.Message.Message.ToolCalls {
			if result, ok := newerResults[call.ID]; ok {
				toolResults[call.ID] = result
			}
		}
		for _, part := range item.Message.Message.Presentation {
			if part.Tool != nil {
				if result, ok := newerResults[part.Tool.Call.ID]; ok {
					toolResults[part.Tool.Call.ID] = result
				}
			}
		}
	}
	p := &model{
		ar: m.ar, width: m.width, height: m.height, scrollview: m.scrollview, sessionState: m.sessionState, subagents: m.subagents,
		toolRenderers: m.toolRenderers, nextResponseGroup: m.nextResponseGroup, backgroundSession: m.backgroundSession,
		inputParentSessionID: m.inputParentSessionID, responseGroups: make(map[*types.Message]uint64),
	}
	p.seedReplayTools(prefix.Messages, toolResults, start)
	previous := m.sessionState.PreviousMessage()
	m.sessionState.SetPreviousMessage(nil)
	r := p.buildSessionReplay(prefix, media, toolResults, start)
	for i, item := range r.items {
		r.apply(i, item)
	}
	m.sessionState.SetPreviousMessage(previous)
	m.nextResponseGroup = p.nextResponseGroup
	maps.Copy(m.responseGroups, p.responseGroups)
	m.restoreTurnTimings(prefix)
	var cmds []tea.Cmd
	for _, view := range p.views {
		if mv, ok := view.(message.Model); ok {
			mv.Finalize()
		}
		cmds = append(cmds, view.Init())
	}
	mergedReasoning := false
	if n := len(p.views); n > 0 && len(m.views) > 0 {
		left, l := p.views[n-1].(*reasoningblock.Model)
		right, r := m.views[0].(*reasoningblock.Model)
		if l && r && p.messages[n-1].Sender == m.messages[0].Sender {
			right.PrependHistory(left)
			mergedReasoning = true
			m.messages[0].Content = p.messages[n-1].Content + "\n\n" + m.messages[0].Content
			m.invalidateItem(0)
			p.views, p.messages = p.views[:n-1], p.messages[:n-1]
		}
	}
	n := len(p.views)
	if n == 0 {
		if mergedReasoning {
			m.layoutViewport()
			m.restoreOlderHistoryAnchor(anchors)
		}
		return tea.Batch(cmds...)
	}
	// Indices into the retained suffix move, but view and message identities do not.
	shift := func(index *int) {
		if *index >= 0 {
			*index += n
		}
	}
	shift(&m.selectedMessageIndex)
	shift(&m.inlineEditMsgIndex)
	shift(&m.inlineEditPrevSelection)
	shift(&m.deferredTailIndex)
	shift(&m.bodyDragIdx)
	m.groupStart += n
	m.loadedMessageCount += n
	m.committedMessageCount += n
	m.messages = slices.Concat(p.messages, m.messages)
	m.views = slices.Concat(p.views, m.views)
	m.indexMessages(0)
	m.historyStart = 0
	m.historyReplay = nil
	m.replyHandoff.Stop()
	m.replyHandoffFrameRows = nil
	m.replyHandoffFrameLines = nil
	m.replyHandoffSerial++
	m.resetPendingReplyHandoff()
	m.workProjection = emptyWorkProjection()
	// Existing blocks retain disclosure by message identity, but their source indices shift.
	for anchor, block := range m.workHeaderBySource {
		m.workBlocks[m.messages[anchor+n]] = block
	}
	m.setWorkHeaders(nil)
	shifted := make(map[int]int, len(m.workSourceToHeader))
	for source, anchor := range m.workSourceToHeader {
		shifted[source+n] = anchor + n
	}
	m.workSourceToHeader = shifted
	m.workProjectionDirty = true
	m.invalidateHistoryStep()
	m.clearSelection()
	m.clearPointerHoverTargets()
	m.CancelReferenceHover()
	m.cancelImageClick()
	m.layoutViewport()
	m.restoreOlderHistoryAnchor(anchors)
	m.layoutViewport()
	return tea.Batch(cmds...)
}

type historyAnchor struct {
	message *types.Message
	local   int
	y       int
}

func (m *model) olderHistoryAnchors() []historyAnchor {
	var anchors []historyAnchor
	for y := range m.bodyHeight() {
		line := m.scrollOffset + y
		if i, local := m.globalLineToMessageLineCanonical(line); i >= 0 && i < len(m.messages) {
			anchors = append(anchors, historyAnchor{m.messages[i], local, y})
		}
	}
	return anchors
}

func (m *model) restoreOlderHistoryAnchor(anchors []historyAnchor) {
	for _, anchor := range anchors {
		i := m.messagePointerIndex(anchor.message)
		if i < 0 {
			continue
		}
		if source, ok := m.workSourceToHeader[i]; ok {
			m.measureSlot(source)
		} else {
			m.measureSlot(i)
		}
		if line := m.sourceToGlobalLine(i, anchor.local); line >= 0 {
			m.userHasScrolled = true
			m.setScrollOffset(line - anchor.y)
			return
		}
	}
}
