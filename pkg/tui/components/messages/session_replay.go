package messages

import (
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
	}
	if !render {
		m.replay = nil
		return true, tea.Batch(cmds...)
	}
	if r.ranges == nil || r.theme != styles.ThemeGeneration() || r.agents != styles.AgentColorGeneration() || r.width != m.contentWidth() {
		r.renderPosition = 0
		r.lines = nil
		r.ranges = make(renderedItemIndex, len(m.views))
		r.offsets = make([]int, len(m.views))
		r.theme = styles.ThemeGeneration()
		r.agents = styles.AgentColorGeneration()
		r.width = m.contentWidth()
	}
	for ops := 0; r.renderPosition < len(m.views) && ops < 32; ops++ {
		i := r.renderPosition
		if view, ok := m.views[i].(message.Model); ok {
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
		item := m.renderItem(i, m.views[i])
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
		if m.needsSeparator(i) {
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
	m.renderedItems = r.ranges
	m.lineOffsets = r.offsets
	m.totalHeight = len(r.lines)
	m.activeSegments = nil
	m.renderDirty = false
	m.themeGeneration = styles.ThemeGeneration()
	m.urlSpans.clear()
	m.replay = nil
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
	if m.replay != msg.replay {
		return
	}
	m.replay.waiting = false
	m.replay.preparedIndex = msg.index
	m.replay.prepared = msg.prepared
	m.replay.preparedReasoning = msg.reasoning
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
