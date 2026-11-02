package messages

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func historyReplayFixture(count int) *session.Session {
	sess := session.New(session.WithID("history-replay"))
	for i := range count {
		sess.Messages = append(sess.Messages, assistantItem("worker", strings.Repeat("paragraph `code` content\n\n", 20)+fmt.Sprintf("history-%04d **result**", i), ""))
	}
	return sess
}

func finishHistoryReplay(tb testing.TB, m *model) {
	tb.Helper()
	for range 10000 {
		if !HistoryPending(m) {
			return
		}
		runReplayCommand(m, ContinueHistory(m))
	}
	tb.Fatal("older history did not finish")
}

func TestReplayPresentsRecentHistoryBeforeRenderingPrefix(t *testing.T) {
	for _, count := range []int{20, 100, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.StopAnimations)
			sess := historyReplayFixture(count)
			BeginReplay(m, PrepareReplay(sess), nil)
			finishPreparedReplay(t, m)
			require.True(t, HistoryPending(m))
			require.Equal(t, count-1, m.historyStart, "the first frame is the tail item alone, since its rows cover the viewport")
			require.GreaterOrEqual(t, m.geom.Height(count-1), m.height, "the tail item's rows cover the viewport, so the suffix is one item")
			_, _, renders := m.ResizeCacheStats()
			require.EqualValues(t, 1, renders, "first usable transcript renders only the covering tail item")
			require.Contains(t, ansi.Strip(m.View()), fmt.Sprintf("history-%04d", count-1))

			m.AddUserMessage("new user input while older history warms")
			m.AppendToLastMessage("worker", "live assistant tail")
			require.Contains(t, ansi.Strip(m.View()), "live assistant tail")
			finishHistoryReplay(t, m)
			require.Zero(t, m.historyStart)
			require.Contains(t, ansi.Strip(m.View()), "live assistant tail")
			m.scrollToTop()
			require.Contains(t, ansi.Strip(m.View()), "paragraph code content")
			require.Equal(t, 0, m.scrollOffset)
			// Old assertion read the eager flattened facade; the lazy layout keeps
			// offscreen bodies unrendered, so the test now makes an explicit full-transcript demand.
			m.ensureAllItemsRendered()
			all := ansi.Strip(strings.Join(m.renderedLines, "\n"))
			last := -1
			for i := range count {
				marker := fmt.Sprintf("history-%04d", i)
				require.Equal(t, 1, strings.Count(all, marker))
				pos := strings.Index(all, marker)
				require.Greater(t, pos, last)
				last = pos
			}
		})
	}
}

// TestHistoryPrefixPagingKeepsAdjacentThinkingGrouped pages history across a merged
// reasoning block. Adjacent reasoning messages from one agent are one block view at
// identity time, so no boundary can fall between the two raw thoughts. Instead the
// block sits on the first-frame boundary: views 0..17 are markers, view 18 is the
// merged block, view 19 is a short tail marker. One page then loads markers 17..10,
// and the block stays one view with both thoughts once, in order.
func TestHistoryPrefixPagingKeepsAdjacentThinkingGrouped(t *testing.T) {
	sess := session.New(session.WithID("history-page-group"))
	for i := range 18 {
		sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: fmt.Sprintf("page marker %02d", i)}))
	}
	thought := func(label string) *session.Message {
		var b strings.Builder
		for line := range 30 {
			fmt.Fprintf(&b, "%s line %02d\n", label, line)
		}
		text := b.String()
		return &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: text, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: text}}}}
	}
	sess.AddMessage(thought("older first thought"))
	sess.AddMessage(thought("older second thought"))
	sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, Content: "page marker tail"}))

	// Expanded thinking makes the merged block tall; collapsed blocks show three preview lines.
	sessionState := &service.SessionState{}
	sessionState.SetExpandThinking(true)
	m := NewScrollableView(animation.NewRuntime(), 80, 12, sessionState).(*model)
	t.Cleanup(m.StopAnimations)
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	require.True(t, HistoryPending(m))
	require.Len(t, m.views, 20, "two adjacent thoughts are one merged view")
	_, isBlock := m.views[18].(*reasoningblock.Model)
	require.True(t, isBlock, "view 18 is the merged reasoning block")
	require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
	require.Equal(t, 18, m.historyStart, "the first frame is the whole merged block plus the tail marker")

	before := m.layoutWorkStats()
	require.Nil(t, ContinueHistory(m), "prefix paging returns no worker command")
	after := m.layoutWorkStats()
	require.Equal(t, before.syncRenders, after.syncRenders, "prefix paging renders no body")
	require.Equal(t, before.compositeViews, after.compositeViews, "prefix paging builds no composite body")
	require.Equal(t, before.prepareScheduled, after.prepareScheduled, "prefix paging schedules no message prepare")
	require.Equal(t, before.prepareExecuted, after.prepareExecuted, "prefix paging executes no message prepare")
	require.Equal(t, before.prepareApplied, after.prepareApplied, "prefix paging applies no message prepare")
	require.Equal(t, before.reasoningScheduled, after.reasoningScheduled, "prefix paging schedules no reasoning prepare")
	require.Equal(t, before.reasoningExecuted, after.reasoningExecuted, "prefix paging executes no reasoning prepare")
	require.Equal(t, before.reasoningApplied, after.reasoningApplied, "prefix paging applies no reasoning prepare")
	require.Equal(t, 10, m.historyStart, "one page loads markers 17..10")
	require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock), "paging does not split the block")

	finishHistoryReplay(t, m)
	// Explicit full demand after the lazy checks above.
	m.ensureAllItemsRendered()
	frame := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	require.Equal(t, 1, strings.Count(frame, "older first thought line 00"))
	require.Equal(t, 1, strings.Count(frame, "older second thought line 00"))
	first, second := strings.Index(frame, "older first thought line 29"), strings.Index(frame, "older second thought line 00")
	require.Greater(t, second, first, "the merged block keeps its thoughts in order")
	require.True(t, m.needsSeparator(17), "the marker before the merged block keeps its separator")
	require.True(t, m.needsSeparator(18), "the merged block before the tail marker keeps its separator")
	require.Contains(t, frame, "page marker 10")
	require.Contains(t, frame, "page marker tail")
}

func TestHistoryReplayPrependsWithoutMovingReaderOrSelection(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	BeginReplay(m, PrepareReplay(historyReplayFixture(50)), nil)
	finishPreparedReplay(t, m)
	m.scrollPageUp()
	before := m.View()
	beforeTotal := m.totalScrollableHeight()
	oldOffset := m.scrollOffset
	m.selection.start(oldOffset+1, 2)
	m.selection.update(oldOffset+2, 5)
	m.selection.anchorTo()
	start, end := m.selection.startLine, m.selection.endLine
	runReplayCommand(m, ContinueHistory(m))
	delta := m.scrollOffset - oldOffset
	require.Positive(t, delta)
	require.Equal(t, start+delta, m.selection.startLine)
	require.Equal(t, end+delta, m.selection.endLine)
	require.Equal(t, start+delta, m.selection.anchorStartLine)
	m.selection.clear()
	after := m.View()
	afterTotal := m.totalScrollableHeight()
	// Content columns keep their text and styling. Only the scrollbar column may change,
	// because the thumb follows the total height, which the prefix grew by its estimate.
	const barColumn, view = 79, 12
	// The cut before the bar column ends with the bar cell's opening style, which is
	// not content; drop that trailing SGR-only run on both sides before comparing.
	trailingSGR := regexp.MustCompile(`(\x1b\[[0-9;]*m)+$`)
	contentCells := func(row string) string {
		return trailingSGR.ReplaceAllString(ansi.Cut(row, 0, barColumn), "")
	}
	beforeRows, afterRows := strings.Split(before, "\n"), strings.Split(after, "\n")
	require.Len(t, beforeRows, view)
	require.Len(t, afterRows, view)
	barBefore, barAfter := make([]string, view), make([]string, view)
	for i := range beforeRows {
		require.Equal(t, 80, ansi.StringWidth(beforeRows[i]), "row %d is 80 cells", i)
		require.Equal(t, 80, ansi.StringWidth(afterRows[i]), "row %d is 80 cells", i)
		require.Equal(t, contentCells(beforeRows[i]), contentCells(afterRows[i]), "row %d keeps its content columns", i)
		barBefore[i], barAfter[i] = ansi.Cut(beforeRows[i], barColumn, 80), ansi.Cut(afterRows[i], barColumn, 80)
	}
	// The thumb is computed from the scroll total, the viewport height and the offset,
	// independently of the rendered bar: height is max(1, view*view/total) and the top
	// is offset*(view-height)/(total-view).
	thumbRowsFor := func(total, offset int) map[int]bool {
		rows := map[int]bool{}
		if total <= view {
			return rows
		}
		height := max(1, view*view/total)
		top := 0
		if maxScroll := total - view; maxScroll > 0 {
			top = offset * (view - height) / maxScroll
		}
		for r := top; r < top+height; r++ {
			rows[r] = true
		}
		return rows
	}
	// The rendered thumb cells are the bar cells whose foreground differs from the track's.
	// The glyph is the same for both; the colour is the cell's last foreground set.
	foreground := regexp.MustCompile(`\x1b\[38;2;(\d+;\d+;\d+)m`)
	cellColor := func(cell string) string {
		all := foreground.FindAllStringSubmatch(cell, -1)
		if len(all) == 0 {
			return ""
		}
		return all[len(all)-1][1]
	}
	renderedThumbRows := func(bar []string) map[int]bool {
		counts := map[string]int{}
		for _, cell := range bar {
			counts[cellColor(cell)]++
		}
		track, most := "", -1
		for color, n := range counts {
			if n > most {
				track, most = color, n
			}
		}
		rows := map[int]bool{}
		for i, cell := range bar {
			if cellColor(cell) != track {
				rows[i] = true
			}
		}
		return rows
	}
	thumbBefore, thumbAfter := thumbRowsFor(beforeTotal, oldOffset), thumbRowsFor(afterTotal, m.scrollOffset)
	require.Equal(t, thumbBefore, renderedThumbRows(barBefore), "the rendered thumb before the prepend matches the computed thumb")
	require.Equal(t, thumbAfter, renderedThumbRows(barAfter), "the rendered thumb after the prepend matches the computed thumb")
	moved := map[int]bool{}
	for i := range barBefore {
		if barBefore[i] != barAfter[i] {
			moved[i] = true
		}
	}
	symmetric := map[int]bool{}
	for r := range thumbBefore {
		if !thumbAfter[r] {
			symmetric[r] = true
		}
	}
	for r := range thumbAfter {
		if !thumbBefore[r] {
			symmetric[r] = true
		}
	}
	require.Equal(t, symmetric, moved, "only bar cells on rows where the thumb moved differ")
}

func TestHistoryReplayResetRejectsInFlightPrefixRender(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	// The visible oversized tail prepares on a worker. Resetting while that result is
	// in flight must reject it.
	sess := historyReplayFixture(14)
	sess.Messages[13] = assistantItem("worker", strings.Repeat("old oversized tail\n\n", 1500), "")
	BeginReplay(m, PrepareReplay(sess), nil)
	var worker tea.Cmd
	for range 100 {
		_, cmd := ContinueReplay(m)
		if ReplayWaiting(m) {
			worker = cmd
			break
		}
		runReplayCommand(m, cmd)
	}
	require.True(t, ReplayWaiting(m))
	require.NotNil(t, worker)
	replacement := historyReplayFixture(12)
	replacement.Messages[11] = assistantItem("worker", "replacement authoritative tail", "")
	BeginReplay(m, PrepareReplay(replacement), nil)
	runReplayCommand(m, worker)
	finishPreparedReplay(t, m)
	finishHistoryReplay(t, m)
	require.Contains(t, ansi.Strip(m.View()), "replacement authoritative tail")
	m.ensureAllItemsRendered()
	require.NotContains(t, strings.Join(m.renderedLines, "\n"), "old oversized tail")
}

func BenchmarkReplayFirstUsable(b *testing.B) {
	for _, count := range []int{20, 100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			sess := historyReplayFixture(count)
			b.ReportAllocs()
			for b.Loop() {
				start := time.Now()
				m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
				BeginReplay(m, PrepareReplay(sess), nil)
				finishPreparedReplay(b, m)
				_ = m.View()
				first := time.Since(start)
				_, _, renders := m.ResizeCacheStats()
				b.ReportMetric(float64(first.Nanoseconds()), "first-usable-ns/op")
				b.ReportMetric(float64(renders), "first-rendered-messages/op")
				m.StopAnimations()
			}
		})
	}
}

func TestHistoryReplayGeometryChangeAndShortCards(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 80, 40, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	sess := historyReplayFixture(100)
	for i := range sess.Messages {
		sess.Messages[i] = assistantItem("worker", fmt.Sprintf("short card %d", i), "")
	}
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	// The first frame is the shortest tail whose actual rendered rows reach the viewport.
	rows := func(from int) int { return m.geom.Total() - m.geom.Offset(from) }
	require.GreaterOrEqual(t, rows(m.historyStart), m.height, "the first frame covers the viewport rows")
	require.Less(t, rows(m.historyStart+1), m.height, "one fewer item would not cover the viewport rows")
	require.GreaterOrEqual(t, m.totalHeight, m.height)
	m.SetSize(60, 20)
	_ = m.View()
	finishHistoryReplay(t, m)
	// Old assertion read the eager flattened facade; the lazy layout keeps offscreen bodies unrendered, so the test now makes an explicit full-transcript demand.
	m.ensureAllItemsRendered()
	all := ansi.Strip(strings.Join(m.renderedLines, "\n"))
	require.Contains(t, all, "short card 0")
	require.Contains(t, ansi.Strip(m.View()), "short card 99")
	for i, offset := range m.lineOffsets {
		if i > 0 {
			require.Greater(t, offset, m.lineOffsets[i-1])
		}
	}
}
