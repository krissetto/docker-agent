package messages

import (
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
	heights                  []int // item height per view, recorded as the replay renders it
	theme                    uint64
	agents                   uint64
	width                    int
	// backward marks the first usable suffix, rendered from the tail upward until
	// the viewport rows are covered. covered counts those rows.
	backward bool
	covered  int
	rendered []bool
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
	if r.heights == nil || r.theme != styles.ThemeGeneration() || r.agents != styles.AgentColorGeneration() || r.width != m.contentWidth() {
		// The first usable frame renders backwards from the tail only until the viewport
		// rows are covered; older items stay identity-only until a viewport demand or
		// ContinueHistory's placeholder paging reaches them.
		// Every render, first or re-render, covers the viewport from the tail. Bodies
		// that a re-render drops are older than the new geometry, so they are dropped
		// before the covering suffix is rendered again.
		m.dropMaterialized()
		r.backward = true
		r.covered = 0
		r.renderPosition = len(m.views) - 1
		r.heights = make([]int, len(m.views))
		r.rendered = make([]bool, len(m.views))
		r.theme = styles.ThemeGeneration()
		r.agents = styles.AgentColorGeneration()
		r.width = m.contentWidth()
	}
	more := func() bool {
		return r.renderPosition >= m.historyStart && r.covered < max(1, m.height)
	}
	if r.backward {
		// The slot index must hold the views before a rendered suffix body is stored,
		// so the finish pass keeps it instead of re-rendering the same visible item.
		m.syncSlots()
	}
	step := func() {
		if r.backward {
			r.renderPosition--
		} else {
			r.renderPosition++
		}
	}
	for ops := 0; more() && ops < 32; ops++ {
		m.syncWorkProjection()
		i := r.renderPosition
		if _, hidden := m.workSourceToHeader[i]; hidden && m.workSourceToHeader[i] != i {
			step()
			continue
		}
		composite := m.workHeaderBySource[i]
		if composite != nil && composite.Expanded() {
			for _, source := range m.workItemsForAnchor(i) {
				if view, ok := source.View.(message.Model); ok {
					view.SetSelected(m.focused && source.SourceIndex == m.selectedMessageIndex)
					view.SetHovered(source.SourceIndex == m.hoveredMessageIndex)
					if r.prepared != nil && r.preparedIndex == source.SourceIndex {
						if message.ApplyPreparedRender(view, r.prepared) {
							m.stats.prepareApplied++
						}
						r.prepared = nil
					}
					if prepare := message.PrepareRender(view); prepare != nil {
						r.waiting = true
						return false, tea.Batch(append(cmds, m.replayPrepareCmd(r, source.SourceIndex, prepare))...)
					}
				}
				if block, ok := source.View.(*reasoningblock.Model); ok {
					if r.preparedReasoning != nil && r.preparedIndex == source.SourceIndex {
						block.ApplyPreparedReasoning(r.preparedReasoning)
						m.stats.reasoningApplied++
						r.preparedReasoning = nil
					}
					if prepare := block.PrepareReasoning(); prepare != nil {
						r.waiting = true
						return false, tea.Batch(append(cmds, m.replayReasoningCmd(r, source.SourceIndex, prepare))...)
					}
				}
			}
		}
		if composite == nil {
			if view, ok := m.views[i].(message.Model); ok {
				view.SetSelected(m.focused && i == m.selectedMessageIndex)
				view.SetHovered(i == m.hoveredMessageIndex)
				if r.prepared != nil && r.preparedIndex == i {
					if message.ApplyPreparedRender(view, r.prepared) {
						m.stats.prepareApplied++
					}
					r.prepared = nil
				}
				if prepare := message.PrepareRender(view); prepare != nil {
					r.waiting = true
					return false, tea.Batch(append(cmds, m.replayPrepareCmd(r, i, prepare))...)
				}
			}
			if block, ok := m.views[i].(*reasoningblock.Model); ok {
				if r.preparedReasoning != nil && r.preparedIndex == i {
					block.ApplyPreparedReasoning(r.preparedReasoning)
					m.stats.reasoningApplied++
					r.preparedReasoning = nil
				}
				if prepare := block.PrepareReasoning(); prepare != nil {
					r.waiting = true
					return false, tea.Batch(append(cmds, m.replayReasoningCmd(r, i, prepare))...)
				}
			}
		}

		var item renderedItem
		if composite != nil {
			m.stats.compositeViews++
			text := m.blockView(composite)
			lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
			item = renderedItem{lines: lines, height: m.blockHeight(composite)}
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
		// Only the height is kept: the slot layout measures rows lazily when they
		// become visible, so the replay never holds a second flattened transcript.
		r.heights[i] = item.height
		r.rendered[i] = true
		if r.backward {
			r.covered += item.height + b2i(m.displayNeedsSeparator(i))
			if composite == nil {
				m.storeReplayBody(i, item)
			}
		}
		step()
		if time.Now().After(deadline) {
			return false, tea.Batch(cmds...)
		}
	}
	if more() {
		return false, tea.Batch(cmds...)
	}
	// Only the first usable render moves the history boundary; a re-render never
	// hides history that is already paged in.
	if r.initial {
		m.historyStart = r.renderPosition + 1
	}
	// The rendered heights become the slot geometry; older rows are estimates until a
	// viewport demand measures them. The bodies rendered above stay materialized.
	m.resetFacade()
	m.slotsStale = true
	// The replay's heights are the geometry; rows are measured on demand, so the
	// layout is not dirty (base finished the same way).
	m.renderDirty = false
	m.replay = nil
	m.themeGeneration = styles.ThemeGeneration()
	m.syncSlots()
	for i, done := range r.rendered {
		if done && !m.hiddenSlot(i) {
			m.setSlotBase(i, r.heights[i])
		}
	}
	m.publishFacade(false)
	m.urlSpans.clear()
	if m.historyStart > 0 && m.historyReplay == nil {
		m.historyReplay = &sessionReplay{}
	}
	m.scrollOffset = max(0, m.layoutTotal()-m.height)
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

// HistoryProgressedMsg asks the page to continue paging older history. It is
// the work-mode continuation signal: ContinueHistory made bounded progress and
// more history remains, so the page must re-arm its paging tick. The page owns
// the tick, so it must handle this message; dropping it stalls paging with
// older history left unmeasured.
type HistoryProgressedMsg struct{}

// ContinueHistory pages older history into the geometry without rendering it, so
// live events and the recent viewport never wait on historical markdown.

func ContinueHistory(list Model) tea.Cmd {
	m := list.(*model)
	r := m.historyReplay
	if r == nil || r.waiting || m.replay != nil {
		return nil
	}
	m.settleGeometry()
	if m.workEnabled {
		oldHeight := m.layoutTotal()
		advanced := 0
		for advanced < 8 && m.historyStart > 0 {
			m.historyStart--
			// Paging a slot in only lifts the zero-height floor that unpaged
			// history sits at; the slot keeps the height it was seeded with while
			// it was still history. Seed the O(1) placeholder the way prependHistory
			// does for the non-work path: a zero-height slot owns no line, so the
			// viewport scan (which enters slots through geom.Find) can never reach
			// it to measure it, and the item stays invisible forever.
			m.seedHistoryEstimate(m.historyStart)
			advanced++
		}
		if advanced == 0 {
			m.historyReplay = nil
			return nil
		}
		if m.historyStart == 0 {
			m.historyReplay = nil
		}
		m.invalidateHistoryStep()
		m.settleGeometry()
		delta := m.layoutTotal() - oldHeight
		if m.userHasScrolled {
			m.scrollOffset = max(0, min(m.layoutTotal()-m.height, m.scrollOffset+delta))
			m.scrollview.SetScrollOffset(m.scrollOffset)
		}
		if m.historyStart == 0 {
			return nil
		}
		return func() tea.Msg { return HistoryProgressedMsg{} }
	}
	// Older history pages in as identity plus the O(1) content estimate. No body is
	// rendered or prepared here; a viewport demand measures the rows it shows.
	deadline := time.Now().Add(2 * time.Millisecond)
	var prefix []int
	for count := 0; m.historyStart-len(prefix) > 0 && count < 8; count++ {
		prefix = append(prefix, m.historyStart-len(prefix)-1)
		if time.Now().After(deadline) {
			break
		}
	}
	m.prependHistory(prefix)
	return nil
}

// invalidateHistoryStep keeps the item cache: renderItem reuses renderedItems
// ranges. The last item is dropped; it may be the streaming suffix, which is
// not in renderedLines.
func (m *model) invalidateHistoryStep() {
	m.workProjectionDirty = true
	m.invalidateFrameContent()
	if last := len(m.views) - 1; last >= 0 {
		m.dropSlot(last)
	}
	m.activeSegments = nil
	m.slotsStale = true
	m.layoutFull = false
	m.urlSpans.clear()
	m.renderDirty = true
	m.invalidateView()
}

// seedHistoryEstimate gives an older item its O(1) placeholder height. The estimate
// is never marked known: the viewport measures the body when it shows the item.
func (m *model) seedHistoryEstimate(i int) {
	if i < 0 || i >= len(m.views) || m.hiddenSlot(i) {
		return
	}
	base := m.placeholderRows(i)
	m.slotBase[i] = base
	m.slotKnown[i] = false
	m.geom.Set(i, base+b2i(m.slotSep[i]))
	m.markFacade()
}

// storeReplayBody keeps the suffix body the replay rendered, so the first frame
// reuses it instead of rendering the same visible item again. The body is stored
// as flattened lines even for a segmented tail: a replayed item is finished, and
// the layout index drops segmented entries at every rebuild. A live tail is still
// measured by View.
func (m *model) storeReplayBody(i int, item renderedItem) {
	lines := m.renderedItemLines(item)
	m.setEntry(i, renderedItemRange{height: len(lines), lines: lines, cacheable: m.cacheableSlot(i)})
}

// replayPrepareCmd hands a message prepare to a worker. Only the atomic executed
// count is touched off the model goroutine.
func (m *model) replayPrepareCmd(r *sessionReplay, index int, prepare func() *message.PreparedRender) tea.Cmd {
	if m.prepareCounts == nil {
		m.prepareCounts = &replayPrepareCounts{}
	}
	counts := m.prepareCounts
	m.stats.prepareScheduled++
	return func() tea.Msg {
		counts.prepareExecuted.Add(1)
		return ReplayRenderedMsg{replay: r, index: index, prepared: prepare()}
	}
}

// replayReasoningCmd is replayPrepareCmd for reasoning blocks.
func (m *model) replayReasoningCmd(r *sessionReplay, index int, prepare func() *reasoningblock.PreparedReasoning) tea.Cmd {
	if m.prepareCounts == nil {
		m.prepareCounts = &replayPrepareCounts{}
	}
	counts := m.prepareCounts
	m.stats.reasoningScheduled++
	return func() tea.Msg {
		counts.reasoningExecuted.Add(1)
		return ReplayRenderedMsg{replay: r, index: index, reasoning: prepare()}
	}
}

func (m *model) prependHistory(prefix []int) {
	if len(prefix) == 0 {
		return
	}
	m.syncSlots()
	before := m.geom.Total()
	m.historyStart = prefix[len(prefix)-1]
	for _, i := range prefix {
		m.seedHistoryEstimate(i)
	}
	// Separators at the new boundary and the materialized rows (now shifted)
	// are rebuilt by the next layout pass; only the visible slots re-render.
	m.slotsStale = true
	m.syncSlots()
	delta := m.geom.Total() - before
	if m.historyStart == 0 {
		m.historyReplay = nil
	}
	m.publishFacade(false)
	m.scrollOffset += delta
	m.consumeGeometryDelta(delta)
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
