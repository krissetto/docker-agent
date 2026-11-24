package messages

import (
	"maps"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// ReplaceHistoryItem replays one canonical item at its placeholder without
// resetting either the retained history or the live tail.
func ReplaceHistoryItem(list Model, position int, item *session.Session, results map[string]string, media map[int][]types.AssistantMedia, failure string) tea.Cmd {
	m := list.(*model)
	i := slices.IndexFunc(m.messages, func(msg *types.Message) bool {
		return msg.Type == types.MessageTypeHistoryPlaceholder && msg.SessionPosition != nil && *msg.SessionPosition == position
	})
	if i < 0 {
		return nil
	}
	if failure != "" {
		msg := m.messages[i]
		msg.Content = failure
		cmd := m.views[i].(message.Model).SetMessage(msg)
		m.invalidateItem(i)
		return cmd
	}

	m.updateScrollState()
	var anchors []historyAnchor
	if m.userHasScrolled {
		anchors = m.olderHistoryAnchors()
	}
	p := &model{
		ar: m.ar, width: m.width, height: m.height, scrollview: m.scrollview, sessionState: m.sessionState, subagents: m.subagents,
		toolRenderers: m.toolRenderers, nextResponseGroup: m.nextResponseGroup, backgroundSession: m.backgroundSession,
		inputParentSessionID: m.inputParentSessionID, responseGroups: make(map[*types.Message]uint64),
	}
	previous := m.sessionState.PreviousMessage()
	if i > 0 {
		m.sessionState.SetPreviousMessage(m.messages[i-1])
	} else {
		m.sessionState.SetPreviousMessage(nil)
	}
	r := p.buildSessionReplay(item, media, results, position)
	r.apply(0, r.items[0])
	m.sessionState.SetPreviousMessage(previous)
	m.nextResponseGroup = p.nextResponseGroup
	maps.Copy(m.responseGroups, p.responseGroups)

	old := m.messages[i]
	// Keep a pointer to any surviving Work header; its source indices move.
	for anchor, block := range m.workHeaderBySource {
		m.workBlocks[m.messages[anchor]] = block
	}
	for source := range m.workSourceToHeader {
		m.views[source].SetSize(m.contentWidth(), 0)
	}
	var cmds []tea.Cmd
	for _, view := range p.views {
		if mv, ok := view.(message.Model); ok {
			mv.Finalize()
		}
		cmds = append(cmds, view.Init())
	}
	m.dropMaterialized()
	m.shiftHistoryRows(i, i+1, len(p.views))
	m.messages = slices.Replace(m.messages, i, i+1, p.messages...)
	m.views = slices.Replace(m.views, i, i+1, p.views...)
	delete(m.messageIndex, old)
	m.indexMessages(i)
	if len(p.messages) > 0 {
		for j := range anchors {
			if anchors[j].message == old {
				anchors[j].message = p.messages[0]
			}
		}
	}
	// A placeholder can separate two parts of one reasoning block. Rejoin
	// only historical neighbors; a live reasoning tail has its own owner.
	for j, end := max(0, i-1), i+len(p.views); j < min(end, m.loadedMessageCount-1); {
		if m.joinHistoryReasoning(j, anchors) {
			end--
		} else {
			j++
		}
	}
	m.workProjection = emptyWorkProjection()
	m.replyHandoff.Stop()
	m.replyHandoffFrameRows = nil
	m.replyHandoffFrameLines = nil
	m.replyHandoffSerial++
	m.resetPendingReplyHandoff()
	m.setWorkHeaders(nil)
	m.workSourceToHeader = nil
	m.workProjectionDirty = true
	m.markSlotsStale()
	m.layoutAnchor = viewportAnchor{}
	m.clearSelection()
	m.clearPointerHoverTargets()
	m.CancelReferenceHover()
	m.cancelImageClick()
	m.invalidateHistoryStep()

	for id, result := range replayToolResults(item.Messages) {
		if _, already := results[id]; !already {
			cmds = append(cmds, m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: id, Response: result}, types.ToolStatusCompleted))
		}
	}
	m.layoutViewport()
	m.restoreOlderHistoryAnchor(anchors)
	m.updateScrollState()
	return tea.Batch(cmds...)
}

func (m *model) shiftHistoryRows(start, end, count int) {
	delta := count - (end - start)
	shiftIndex := func(index *int) {
		switch {
		case *index >= end:
			*index += delta
		case *index >= start:
			*index = -1
		}
	}
	for _, index := range []*int{&m.selectedMessageIndex, &m.inlineEditMsgIndex, &m.inlineEditPrevSelection, &m.deferredTailIndex, &m.bodyDragIdx} {
		shiftIndex(index)
	}
	for _, boundary := range []*int{&m.groupStart, &m.loadedMessageCount, &m.committedMessageCount, &m.historyStart} {
		if *boundary >= end {
			*boundary += delta
		}
	}
}

func (m *model) joinHistoryReasoning(i int, anchors []historyAnchor) bool {
	left, l := m.views[i].(*reasoningblock.Model)
	right, r := m.views[i+1].(*reasoningblock.Model)
	if !l || !r || m.messages[i].Sender != m.messages[i+1].Sender {
		return false
	}
	old, kept := m.messages[i], m.messages[i+1]
	right.PrependHistory(left)
	kept.Content = old.Content + "\n\n" + kept.Content
	for j := range anchors {
		if anchors[j].message == old {
			anchors[j].message = kept
		}
	}
	m.shiftHistoryRows(i, i+1, 0)
	animation.StopView(left)
	delete(m.messageIndex, old)
	m.views = slices.Delete(m.views, i, i+1)
	m.messages = slices.Delete(m.messages, i, i+1)
	m.indexMessages(i)
	return true
}
