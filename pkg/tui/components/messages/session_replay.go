package messages

import (
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/message"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

type sessionReplay struct {
	preparedReasoning        *reasoningblock.PreparedReasoning
	initial                  bool
	renderOnly               bool
	waiting                  bool
	preparedIndex            int
	prepared                 *message.PreparedRender
	items                    []session.Item
	apply                    func(int, session.Item)
	position, renderPosition int
	lines                    []string
	ranges                   renderedItemIndex
	offsets                  []int
	theme                    uint64
	agents                   uint64
	width                    int
}

// PreparedReplay is immutable after preparation and belongs to one replay.
type PreparedReplay struct {
	Session     *session.Session
	toolResults map[string]string
}

func PrepareReplay(sess *session.Session) PreparedReplay {
	snapshot := sess.Clone()
	return PreparedReplay{Session: snapshot, toolResults: replayToolResults(snapshot.Messages)}
}

func replayToolResults(items []session.Item) map[string]string {
	results := make(map[string]string)
	for _, item := range items {
		if item.IsMessage() && lifecycle.VisibleTranscriptMessage(item.Message) && item.Message.Message.Role == chat.MessageRoleTool && item.Message.Message.ToolCallID != "" {
			results[item.Message.Message.ToolCallID] = item.Message.Message.Content
		}
	}
	return results
}

func BeginReplay(list Model, prepared PreparedReplay, media map[int][]types.AssistantMedia) {
	m := list.(*model)
	m.startSessionReplay(prepared.Session, media, prepared.toolResults)
	m.replay.initial = true
}

func ContinueReplay(list Model) (bool, tea.Cmd) { return list.(*model).applySessionReplay(true) }

func (m *model) applySessionReplay(render bool) (bool, tea.Cmd) {
	r := m.replay
	if r == nil {
		return true, nil
	}
	if r.waiting {
		return false, nil
	}
	deadline := time.Now().Add(4 * time.Millisecond)
	var cmds []tea.Cmd
	for ops := 0; r.position < len(r.items) && ops < 32; ops++ {
		before := len(m.views)
		r.apply(r.position, r.items[r.position])
		r.position++
		for i := before; i < len(m.views); i++ {
			cmds = append(cmds, m.views[i].Init())
			if mv, ok := m.views[i].(message.Model); ok && r.position < len(r.items) {
				mv.Finalize()
			}
		}
		if time.Now().After(deadline) {
			return false, tea.Batch(cmds...)
		}
	}
	if r.position < len(r.items) {
		return false, tea.Batch(cmds...)
	}
	if !r.renderOnly {
		m.loadedMessageCount, m.committedMessageCount = len(m.messages), len(m.messages)
		m.workProjectionDirty = true
		m.syncWorkProjection()
	}
	if !render {
		m.replay = nil
		return true, tea.Batch(cmds...)
	}
	if r.ranges == nil || r.theme != styles.ThemeGeneration() || r.agents != styles.AgentColorGeneration() || r.width != m.contentWidth() {
		if r.initial {
			// A minimum row per view fills the first viewport even for short cards.
			m.historyStart = max(0, len(m.views)-max(8, m.height))
		}
		r.renderPosition = m.historyStart
		r.lines = nil
		r.ranges = make(renderedItemIndex, len(m.views))
		r.offsets = make([]int, len(m.views))
		r.theme = styles.ThemeGeneration()
		r.agents = styles.AgentColorGeneration()
		r.width = m.contentWidth()
	}
	for ops := 0; r.renderPosition < len(m.views) && ops < 32; ops++ {
		m.syncWorkProjection()
		i := r.renderPosition
		if _, hidden := m.workSourceToHeader[i]; hidden && m.workSourceToHeader[i] != i {
			r.offsets[i] = len(r.lines)
			r.renderPosition++
			continue
		}
		composite := m.workHeaderBySource[i]
		if composite != nil && composite.Expanded() {
			for _, source := range m.workItemsForAnchor(i) {
				if view, ok := source.View.(message.Model); ok {
					view.SetSelected(m.focused && source.SourceIndex == m.selectedMessageIndex)
					view.SetHovered(source.SourceIndex == m.hoveredMessageIndex)
					if r.prepared != nil && r.preparedIndex == source.SourceIndex {
						message.ApplyPreparedRender(view, r.prepared)
						r.prepared = nil
					}
					if prepare := message.PrepareRender(view); prepare != nil {
						r.waiting = true
						return false, tea.Batch(append(cmds, func() tea.Msg { return ReplayRenderedMsg{replay: r, index: source.SourceIndex, prepared: prepare()} })...)
					}
				}
				if block, ok := source.View.(*reasoningblock.Model); ok {
					if r.preparedReasoning != nil && r.preparedIndex == source.SourceIndex {
						block.ApplyPreparedReasoning(r.preparedReasoning)
						r.preparedReasoning = nil
					}
					if prepare := block.PrepareReasoning(); prepare != nil {
						r.waiting = true
						return false, tea.Batch(append(cmds, func() tea.Msg { return ReplayRenderedMsg{replay: r, index: source.SourceIndex, reasoning: prepare()} })...)
					}
				}
			}
		}
		if composite == nil {
			if view, ok := m.views[i].(message.Model); ok {
				view.SetSelected(m.focused && i == m.selectedMessageIndex)
				view.SetHovered(i == m.hoveredMessageIndex)
				if r.prepared != nil && r.preparedIndex == i {
					message.ApplyPreparedRender(view, r.prepared)
					r.prepared = nil
				}
				if prepare := message.PrepareRender(view); prepare != nil {
					r.waiting = true
					return false, tea.Batch(append(cmds, func() tea.Msg { return ReplayRenderedMsg{replay: r, index: i, prepared: prepare()} })...)
				}
			}
			if block, ok := m.views[i].(*reasoningblock.Model); ok {
				if r.preparedReasoning != nil && r.preparedIndex == i {
					block.ApplyPreparedReasoning(r.preparedReasoning)
					r.preparedReasoning = nil
				}
				if prepare := block.PrepareReasoning(); prepare != nil {
					r.waiting = true
					return false, tea.Batch(append(cmds, func() tea.Msg { return ReplayRenderedMsg{replay: r, index: i, reasoning: prepare()} })...)
				}
			}
		}

		var item renderedItem
		if composite != nil {
			text := composite.View()
			lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
			item = renderedItem{lines: lines, height: composite.Height()}
		} else {
			item = m.renderItem(i, m.views[i])
		}
		if composite != nil {
			for _, source := range m.workItemsForAnchor(i) {
				if view, ok := source.View.(message.Model); ok {
					message.ReleasePreparedRender(view)
				}
			}
		} else if view, ok := m.views[i].(message.Model); ok {
			// The transcript now owns these lines. Do not retain a second full
			// ANSI document on every historical (or last replayed) message view.
			message.ReleasePreparedRender(view)
		}
		start := len(r.lines)
		r.offsets[i] = start
		if item.segments != nil {
			r.lines = append(r.lines, item.segments.Header...)
			r.lines = append(r.lines, item.segments.Stable...)
			r.lines = append(r.lines, item.segments.Tail...)
		} else {
			r.lines = append(r.lines, item.lines...)
		}
		if m.shouldCacheMessage(i) {
			r.ranges[i] = renderedItemRange{start: start, height: item.height}
		}
		if m.displayNeedsSeparator(i) {
			r.lines = append(r.lines, "")
		}
		r.renderPosition++
		if time.Now().After(deadline) {
			return false, tea.Batch(cmds...)
		}
	}
	if r.renderPosition < len(m.views) {
		return false, tea.Batch(cmds...)
	}
	m.renderedLines = r.lines
	m.dropRenderedBacking()
	m.renderedItems = r.ranges
	m.lineOffsets = r.offsets
	m.totalHeight = len(r.lines)
	m.activeSegments = nil
	m.renderDirty = false
	m.themeGeneration = styles.ThemeGeneration()
	m.urlSpans.clear()
	m.replay = nil
	if m.historyStart > 0 && m.historyReplay == nil {
		m.historyReplay = &sessionReplay{}
	}
	m.scrollOffset = max(0, m.totalHeight-m.height)
	m.scrollview.SetScrollOffset(m.scrollOffset)
	m.invalidateView()
	return true, tea.Batch(cmds...)
}

// ReplayRenderedMsg is routed back to the page that owns this replay.
type ReplayRenderedMsg struct {
	reasoning *reasoningblock.PreparedReasoning
	replay    *sessionReplay
	index     int
	prepared  *message.PreparedRender
}

func ReplayWaiting(list Model) bool { m := list.(*model); return m.replay != nil && m.replay.waiting }
func ApplyReplayRender(list Model, msg ReplayRenderedMsg) {
	m := list.(*model)
	if m.replay != msg.replay && m.historyReplay != msg.replay {
		return
	}
	msg.replay.waiting = false
	msg.replay.preparedIndex = msg.index
	msg.replay.prepared = msg.prepared
	msg.replay.preparedReasoning = msg.reasoning
}

// ReconcileReplay prepares invalidated geometry without reloading the snapshot.
func ReconcileReplay(list Model) bool {
	m := list.(*model)
	if !m.renderDirty && m.themeGeneration == styles.ThemeGeneration() {
		return false
	}
	m.replay = &sessionReplay{renderOnly: true}
	return true
}

// HistoryPending reports retained older views not yet added to transcript geometry.
func HistoryPending(list Model) bool { return list.HistoryPending() }

func (m *model) HistoryPending() bool { return m.historyReplay != nil }

// ContinueHistory warms older rows separately from authoritative replay so live
// events and the recent viewport never wait for historical markdown rendering.
type historyProgressedMsg struct{}

func ContinueHistory(list Model) tea.Cmd {
	m := list.(*model)
	r := m.historyReplay
	if r == nil || r.waiting || m.replay != nil {
		return nil
	}
	m.ensureAllItemsRendered()
	if m.workEnabled {
		oldHeight := m.totalHeight
		advanced := 0
		for advanced < 8 && m.historyStart > 0 {
			m.historyStart--
			advanced++
		}
		if advanced == 0 {
			m.historyReplay = nil
			return nil
		}
		if m.historyStart == 0 {
			m.historyReplay = nil
			m.releaseRenderedHeadroom()
		}
		m.invalidateHistoryStep()
		m.ensureAllItemsRendered()
		delta := m.totalHeight - oldHeight
		if m.userHasScrolled {
			m.scrollOffset = max(0, min(m.totalHeight-m.height, m.scrollOffset+delta))
			m.scrollview.SetScrollOffset(m.scrollOffset)
		}
		if m.historyStart == 0 {
			return nil
		}
		return func() tea.Msg { return historyProgressedMsg{} }
	}
	var prefix []historyItem
	defer func() { m.prependHistory(prefix) }()
	deadline := time.Now().Add(2 * time.Millisecond)
	for count := 0; m.historyStart-len(prefix) > 0 && count < 8; count++ {
		i := m.historyStart - len(prefix) - 1
		if view, ok := m.views[i].(message.Model); ok {
			view.SetSelected(false)
			view.SetHovered(false)
			if r.prepared != nil && r.preparedIndex == i {
				message.ApplyPreparedRender(view, r.prepared)
				r.prepared = nil
			}
			if prepare := message.PrepareRender(view); prepare != nil {
				r.waiting = true
				return func() tea.Msg { return ReplayRenderedMsg{replay: r, index: i, prepared: prepare()} }
			}
		}
		if block, ok := m.views[i].(*reasoningblock.Model); ok {
			block.SetSelected(false)
			if r.preparedReasoning != nil && r.preparedIndex == i {
				block.ApplyPreparedReasoning(r.preparedReasoning)
				r.preparedReasoning = nil
			}
			if prepare := block.PrepareReasoning(); prepare != nil {
				r.waiting = true
				return func() tea.Msg { return ReplayRenderedMsg{replay: r, index: i, reasoning: prepare()} }
			}
		}
		item := m.renderItem(i, m.views[i])
		if view, ok := m.views[i].(message.Model); ok {
			message.ReleasePreparedRender(view)
		}
		lines := item.lines
		if item.segments != nil {
			lines = append(append(append([]string(nil), item.segments.Header...), item.segments.Stable...), item.segments.Tail...)
		}
		if item.height > 0 && m.needsSeparator(i) {
			lines = append(lines, "")
		}
		prefix = append(prefix, historyItem{index: i, lines: lines, height: item.height})
		if time.Now().After(deadline) {
			break
		}
	}
	return nil
}

// invalidateHistoryStep keeps the item cache: renderItem reuses renderedItems
// ranges. The last item is dropped; it may be the streaming suffix, which is
// not in renderedLines.
func (m *model) invalidateHistoryStep() {
	m.workProjectionDirty = true
	m.invalidateFrameContent()
	if last := len(m.views) - 1; last >= 0 {
		m.renderedItems.Delete(last)
	}
	m.activeSegments = nil
	m.lineOffsets = nil
	m.urlSpans.clear()
	m.renderDirty = true
	m.invalidateView()
}

// prependRenderedLines writes a batch in front of renderedLines using front
// headroom, so each batch copies only its own rows.
func (m *model) prependRenderedLines(prefix []historyItem, delta int) {
	current := len(m.renderedLines)
	if !m.renderedLinesOwnBacking() || m.renderedHead < delta {
		room := max(delta, current)
		backing := make([]string, room+current)
		copy(backing[room:], m.renderedLines)
		m.renderedBacking, m.renderedHead = backing, room
	}
	head := m.renderedHead - delta
	pos := head
	for _, item := range slices.Backward(prefix) {
		pos += copy(m.renderedBacking[pos:], item.lines)
	}
	m.renderedHead = head
	m.renderedLines = m.renderedBacking[head:]
}

// renderedLinesOwnBacking reports whether renderedLines is still the window of renderedBacking.
func (m *model) renderedLinesOwnBacking() bool {
	n := len(m.renderedLines)
	if m.renderedHead < 0 || m.renderedHead > len(m.renderedBacking) || len(m.renderedBacking)-m.renderedHead != n {
		return false
	}
	return n == 0 || &m.renderedBacking[m.renderedHead] == &m.renderedLines[0]
}

// releaseRenderedHeadroom compacts the window to an exact-size transcript.
func (m *model) releaseRenderedHeadroom() {
	if m.renderedLinesOwnBacking() && m.renderedHead > 0 {
		m.renderedLines = slices.Clone(m.renderedLines)
	}
	m.dropRenderedBacking()
}

// dropRenderedBacking must run wherever renderedLines is replaced or cleared.
func (m *model) dropRenderedBacking() { m.renderedBacking, m.renderedHead = nil, 0 }

type historyItem struct {
	index, height int
	lines         []string
}

func (m *model) prependHistory(prefix []historyItem) {
	if len(prefix) == 0 {
		return
	}
	delta := 0
	for _, item := range prefix {
		delta += len(item.lines)
	}
	m.prependRenderedLines(prefix, delta)
	for i := m.historyStart; i < len(m.lineOffsets); i++ {
		m.lineOffsets[i] += delta
	}
	for i, cached := range m.renderedItems {
		cached.start += delta
		m.renderedItems[i] = cached
	}
	offset := 0
	for i := len(prefix) - 1; i >= 0; i-- {
		item := prefix[i]
		m.lineOffsets[item.index] = offset
		if m.shouldCacheMessage(item.index) {
			m.renderedItems[item.index] = renderedItemRange{start: offset, height: item.height}
		}
		offset += len(item.lines)
	}
	m.historyStart = prefix[len(prefix)-1].index
	if m.historyStart == 0 {
		m.historyReplay = nil
		m.releaseRenderedHeadroom()
	}
	if m.activeSegments != nil {
		m.activeSegments.start += delta
	}
	m.totalHeight += delta
	m.scrollOffset += delta
	m.scrollview.SetScrollOffset(m.scrollOffset)
	if m.selection.active {
		m.selection.startLine += delta
		m.selection.endLine += delta
		m.selection.anchorStartLine += delta
		m.selection.anchorEndLine += delta
	}
	m.CancelReferenceHover()
	m.cancelImageClick()
	m.urlSpans.clear()
	m.invalidateFrameContent()
	m.invalidateView()
}
