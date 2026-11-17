package messages

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func stickyWorkHistory(t *testing.T) (*model, []int) {
	t.Helper()
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 64, 10, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.suppressReplyHandoff = true
	var anchors []int
	for turn := range 5 {
		m.addMessage(types.User(fmt.Sprintf("request %d", turn)))
		anchors = append(anchors, len(m.messages))
		m.addMessage(types.Agent(types.MessageTypeAssistantReasoningBlock, "root", strings.Repeat(fmt.Sprintf("thinking %d\n\n", turn), 12)))
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Repeat(fmt.Sprintf("answer %d\n\n", turn), 20)))
	}
	m.ensureAllItemsRendered()
	m.userHasScrolled = true
	return m, anchors
}

func TestStickyWorkSectionsAcrossHistoryWithoutGeometryChanges(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	for _, anchor := range anchors {
		start := m.slotOffset(anchor)
		m.setScrollOffset(start)
		_, _, ok := m.stickyWork()
		require.False(t, ok, "natural header owns its row")
		m.setScrollOffset(start + 3)
		beforeTotal, beforeOffset, beforeSlack := m.layoutTotal(), m.scrollOffset, m.bottomSlack
		source, block, ok := m.stickyWork()
		require.True(t, ok)
		require.Equal(t, anchor, source)
		frame := strings.Split(ansi.Strip(m.View()), "\n")
		require.Contains(t, frame[0], "Work [+]")
		require.Equal(t, block.HeaderView(), m.transcriptFrameKey().stickyWorkHeader)
		require.Equal(t, beforeTotal, m.layoutTotal())
		require.Equal(t, beforeOffset, m.scrollOffset)
		require.Equal(t, beforeSlack, m.bottomSlack)
		require.Len(t, frame, m.height)
	}
	// Moving upward into a previous section restores that section's identity;
	// user input boundaries never inherit the preceding section's sticky row.
	for i := len(anchors) - 1; i >= 0; i-- {
		m.setScrollOffset(m.slotOffset(anchors[i] - 1))
		_, _, ok := m.stickyWork()
		require.False(t, ok)
		m.setScrollOffset(m.slotOffset(anchors[i]) + 3)
		source, _, ok := m.stickyWork()
		require.True(t, ok)
		require.Equal(t, anchors[i], source)
	}
}

func TestStickyWorkPointerKeyboardAndReaderAnchor(t *testing.T) {
	for _, keyboard := range []bool{false, true} {
		t.Run(fmt.Sprint("keyboard=", keyboard), func(t *testing.T) {
			m, anchors := stickyWorkHistory(t)
			anchor := anchors[1]
			block := m.workHeaderBySource[anchor]
			m.setScrollOffset(m.slotOffset(anchor) + 5)
			line, col := m.mouseToLineCol(m.xPos+3, m.yPos)
			require.Equal(t, -1, line)
			require.Equal(t, -1, col)
			_, _, ok := m.stickyWorkAt(m.xPos+m.contentWidth(), m.yPos)
			require.False(t, ok, "scrollbar is not obscured")
			if keyboard {
				m.focused, m.selectedMessageIndex = true, anchor
				m.handleKeyPress(tea.KeyPressMsg{Code: tea.KeyEnter})
			} else {
				m.handleMouseClick(tea.MouseClickMsg{X: m.xPos + 3, Y: m.yPos, Button: tea.MouseLeft})
			}
			require.True(t, block.Expanded())
			require.Equal(t, m.slotOffset(anchor), m.scrollOffset)
			require.False(t, m.selection.mouseButtonDown)
			block.SetExpandedImmediately(true)
			m.invalidateItem(anchor)
			m.ensureAllItemsRendered()
			m.setScrollOffset(m.slotOffset(anchor+1) + 4)
			source, local := m.globalLineToMessageLineCanonical(m.scrollOffset)
			m.activateStickyWork(block)
			require.False(t, block.Expanded())
			require.Equal(t, m.sourceToGlobalLine(source, local), m.scrollOffset, "folding preserves the reply row under the overlay")
		})
	}
}

func TestStickyWorkLoadingResizeAndSelection(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	m.setScrollOffset(m.slotOffset(anchors[1]) + 4)
	_, _, ok := m.stickyWork()
	require.True(t, ok)
	line, col := m.selectionMouseToLineCol(m.xPos+20, m.yPos)
	require.Equal(t, m.scrollOffset+1, line)
	require.Zero(t, col)
	m.replay = &sessionReplay{}
	_, _, ok = m.stickyWork()
	require.False(t, ok)
	m.replay = nil
	m.historyReplay = &sessionReplay{}
	_, _, ok = m.stickyWork()
	require.True(t, ok, "known loaded geometry remains usable while older history pages")
	m.historyReplay = nil
	m.SetSize(12, 5)
	_, _, ok = m.stickyWork()
	require.False(t, ok)
	m.SetSize(12, 8)
	m.ensureAllItemsRendered()
	m.setScrollOffset(m.slotOffset(anchors[1]) + 4)
	require.LessOrEqual(t, ansi.StringWidth(strings.Split(m.View(), "\n")[0]), m.width)
}

func TestStickyWorkTracksAnimatedHeaderPosition(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	anchor := anchors[1]
	block := m.workHeaderBySource[anchor]
	m.setScrollOffset(m.slotOffset(anchor) + 4)
	// Activate a minimal handoff with a header at the visible terminal row.
	m.replyHandoff.phase = replyHandoffReflow
	m.replyHandoffFrameRows = []replyHandoffFrameRow{{Composite: block, CompositeAnchor: anchor, CompositeLine: 0, Y: float64(m.scrollOffset), Height: 1}}
	_, _, ok := m.stickyWork()
	require.False(t, ok)
	m.replyHandoffFrameRows[0].Y = float64(m.scrollOffset) - 1
	_, _, ok = m.stickyWork()
	require.False(t, ok, "handoff latches visibility until completion")
	m.replyHandoff.Stop()
	_, got, ok := m.stickyWork()
	require.True(t, ok)
	require.Same(t, block, got)
}

func TestStickyWorkExcludesObscuredRowFromSelectionAndImages(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	m.setScrollOffset(m.slotOffset(anchors[1]) + 5)
	m.selection.start(m.scrollOffset+1, 0)
	m.selection.update(m.scrollOffset+2, 20)
	_, _, ok := m.stickyWork()
	require.False(t, ok, "an active text range reveals all underlying rows")
	m.selection.clear()
	// Real image markers are clipped by replacing the obscured painted row.
	inline := generatedMediaTestImage(t, "sticky.png")
	m.AppendAssistantMedia("root", []types.AssistantMedia{{ID: 1, Image: &inline}})
	m.ensureAllItemsRendered()
	marker := -1
	for line := m.slotOffset(anchors[len(anchors)-1]); line < m.layoutTotal(); line++ {
		if strings.Contains(m.renderedLine(line), "\x1b_cagent-image;") {
			marker = line
			break
		}
	}
	require.NotEqual(t, -1, marker)
	m.setScrollOffset(marker)
	frame := strings.Split(m.View(), "\n")
	require.NotContains(t, frame[0], "\x1b_cagent-image;")
	require.Contains(t, ansi.Strip(frame[0]), "Work [+]")
	require.Nil(t, m.imageClick)
}

func TestStickyWorkSectionEndBudgetAndNarrowWidths(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	anchor := anchors[1]
	exchange := m.workProjection.exchanges[1]
	last := exchange.response[len(exchange.response)-1]
	end := m.slotOffset(last) + m.slotBase[last]
	for _, remaining := range []int{4, 3, 2, 1} {
		m.setScrollOffset(end - remaining)
		_, _, ok := m.stickyWork()
		require.Equal(t, remaining >= 3, ok, "need sticky plus two same-section rows")
	}
	m.SetSize(15, 8)
	m.ensureAllItemsRendered()
	m.setScrollOffset(m.slotOffset(anchor) + 4)
	_, _, ok := m.stickyWork()
	require.False(t, ok)
	m.SetSize(64, 8)
	m.ensureAllItemsRendered()
	m.setScrollOffset(m.slotOffset(anchor) + 4)
	_, _, ok = m.stickyWork()
	require.True(t, ok)
}

func TestStickyWorkShortSectionsStayNatural(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	anchor := anchors[1]
	// A measured short section cannot justify spending one viewport row.
	last := m.workProjection.exchanges[1].response[0]
	m.slotBase[last] = 2
	m.geom.Set(last, 2)
	m.setScrollOffset(m.slotOffset(anchor) + 1)
	_, _, ok := m.stickyWork()
	require.False(t, ok)
}

func TestStickyWorkPinnedReplyFoldAndInputHitExclusion(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	anchor := anchors[len(anchors)-1]
	block := m.workHeaderBySource[anchor]
	block.SetExpandedImmediately(true)
	m.invalidateItem(anchor)
	m.ensureAllItemsRendered()
	m.userHasScrolled = false
	m.scrollToBottom()
	m.View()
	_, _, ok := m.stickyWork()
	require.True(t, ok)
	_, found := m.InputReferenceAt(m.xPos+3, m.yPos)
	require.False(t, found)
	_, found = m.SubagentNodeAt(m.xPos+3, m.yPos)
	require.False(t, found)
	m.activateStickyWork(block)
	m.View()
	require.False(t, m.userHasScrolled, "folding from a pinned reply retains follow")
	require.Equal(t, max(0, m.totalScrollableHeight()-m.height), m.scrollOffset)
}
