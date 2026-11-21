package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func stickyPreviewModel(t *testing.T, height int, answer string) (*model, int) {
	t.Helper()
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 64, height, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.suppressReplyHandoff = true
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendReasoning("root", "preview one\n\npreview two\n\npreview three")
	m.syncWorkProjection()
	anchor := m.workProjection.exchanges[0].work[0]
	if answer != "" {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", answer))
	}
	m.views[anchor].(*reasoningblock.Model).SetReasoningActive(true)
	m.invalidateItem(anchor)
	m.ensureAllItemsRendered()
	block := m.workHeaderBySource[anchor]
	block.StopAnimation()
	m.invalidateItem(anchor)
	m.measureSlot(anchor)
	require.Equal(t, 4, m.slotBase[anchor])
	return m, anchor
}

func TestStickyWorkInsertedPreviewRowsAndHits(t *testing.T) {
	for _, height := range []int{4, 5, 6, 10} {
		t.Run(fmt.Sprint(height), func(t *testing.T) {
			m, anchor := stickyPreviewModel(t, height, strings.Repeat("answer\n\n", 20))
			m.userHasScrolled = true
			start := m.slotOffset(anchor)
			for _, local := range []int{1, 2, 3, 4, 3, 2, 1} {
				m.setScrollOffset(start + local)
				total, visits, views := m.layoutTotal(), m.stats.itemVisits, m.stats.blockViews
				_, block, ok := m.stickyWorkAt(m.xPos+3, m.yPos)
				require.True(t, ok)
				require.Equal(t, total, m.layoutTotal())
				require.Equal(t, visits, m.stats.itemVisits)
				require.Equal(t, views, m.stats.blockViews)
				frame := strings.Split(ansi.Strip(m.View()), "\n")
				require.Len(t, frame, height)
				require.Contains(t, frame[0], "Working [+]")
				for y := 1; y < height; y++ {
					require.Equal(t, strings.TrimSpace(ansi.Strip(m.renderedLine(start+local+y-1))), strings.TrimSpace(strings.TrimSuffix(frame[y], "│")))
				}
				if local == 1 {
					for _, text := range []string{"preview one", "preview two", "preview three"} {
						require.Contains(t, strings.Join(frame, "\n"), text)
					}
				}
				line, col := m.mouseToLineCol(m.xPos+3, m.yPos)
				require.Equal(t, -1, line)
				require.Equal(t, -1, col)
				line, col = m.mouseToLineCol(m.xPos+3, m.yPos+1)
				require.Equal(t, start+local, line)
				require.Equal(t, 3, col)
				line, col = m.selectionMouseToLineCol(m.xPos+3, m.yPos)
				require.Equal(t, start+local, line)
				require.Zero(t, col)
				_, _, ok = m.stickyWorkAt(m.xPos+3, m.yPos+1)
				require.False(t, ok)
				require.Same(t, m.workHeaderBySource[anchor], block)
			}
			m.activateStickyWork(m.workHeaderBySource[anchor])
			require.Equal(t, m.slotOffset(anchor), m.scrollOffset)
			require.True(t, m.workHeaderBySource[anchor].Expanded())
		})
	}
}

func TestStickyWorkBottomPinnedLivePreviewAndGrowth(t *testing.T) {
	for _, height := range []int{4, 5, 6} {
		t.Run(fmt.Sprint(height), func(t *testing.T) {
			m, anchor := stickyPreviewModel(t, height, strings.Repeat("answer\n", max(0, height-5)))
			m.scrollToBottom()
			frame := ansi.Strip(m.View())
			for _, text := range []string{"Working [+]", "preview one", "preview two", "preview three"} {
				require.Contains(t, frame, text)
			}
			for rows := 1; rows <= 12; rows++ {
				if len(m.messages) == anchor+1 {
					m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "answer"))
				}
				last := len(m.messages) - 1
				m.messages[last].Content = strings.Repeat("answer\n", rows)
				m.views[last] = m.createMessageView(m.messages[last])
				m.invalidateItem(last)
				m.layoutViewport()
				frame = strings.Join(strings.Split(ansi.Strip(m.View()), "\n"), "\n")
				require.False(t, m.userHasScrolled)
				require.Equal(t, max(0, m.totalScrollableHeight()-height), m.scrollOffset)
				require.Contains(t, frame, "answer")
				require.Equal(t, m.displayTotalHeight()+m.bottomSlack+m.scrollOnSend.padding, m.scrollOffset+m.bodyHeight(), "last canonical row remains reachable")
				offset := m.scrollOffset
				m.View()
				m.View()
				require.Equal(t, offset, m.scrollOffset, "reserve must not flip-flop")
			}
			before := m.scrollOffset
			m.addMessage(types.User("next request"))
			m.View()
			require.Equal(t, max(0, m.totalScrollableHeight()-height), m.scrollOffset)
			require.False(t, m.userHasScrolled)
			require.GreaterOrEqual(t, m.scrollOffset, before-1)
		})
	}
}

func TestStickyWorkInsetHandoffReaderAnchor(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	m.setScrollOffset(m.slotOffset(anchors[1]+1) + 4)
	require.Equal(t, 1, m.stickyWorkInset())
	source, local := m.globalLineToMessageLineCanonical(m.scrollOffset)
	m.captureReplyHandoffAnchor()
	anchor, ok := m.replyHandoffAnchors[m.messages[source]]
	require.True(t, ok)
	require.Equal(t, 0, anchor.screenY, "handoff anchors are body-relative even with an inserted header")
	require.Equal(t, local, anchor.localRow)
	before := m.scrollOffset
	m.restoreReplyHandoffAnchor([]replyHandoffFrameRow{{Source: m.messages[source], LocalRow: local, Interactive: true, Y: float64(before + 2), Height: 1}})
	require.Equal(t, before+2, m.scrollOffset)
}

func TestStickyWorkInsetPageAndScrollbarGeometry(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	start := m.slotOffset(anchors[1]) + 5
	m.setScrollOffset(start)
	require.Equal(t, m.height-1, m.bodyHeight())
	m.scrollPageDown()
	require.Equal(t, start+m.height-1, m.scrollOffset)
	m.scrollPageUp()
	require.Equal(t, start, m.scrollOffset)
	m.scrollToBottom()
	m.View()
	require.Equal(t, max(0, m.totalScrollableHeight()-m.height), m.scrollOffset)
	require.Equal(t, m.scrollOffset, m.scrollview.ScrollOffset())
	require.True(t, m.isAtBottom())
}

func TestStickyWorkBelowFourRowsClipsNaturally(t *testing.T) {
	for _, height := range []int{1, 2, 3} {
		m, anchor := stickyPreviewModel(t, height, strings.Repeat("answer\n\n", 20))
		m.userHasScrolled = true
		m.setScrollOffset(m.slotOffset(anchor) + 1)
		_, _, ok := m.stickyWork()
		require.False(t, ok)
		frame := strings.Split(ansi.Strip(m.View()), "\n")
		require.Len(t, frame, height)
		require.Contains(t, frame[0], "preview one")
	}
}

func TestStickyWorkCachedHeaderTracksVisualGeneration(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	anchor := anchors[1]
	m.setScrollOffset(m.slotOffset(anchor) + 5)
	m.View()
	before := m.lastFrameKey
	require.NotEmpty(t, before.stickyWorkHeader)
	m.setScrollOffset(m.scrollOffset + 1)
	frame := m.transcriptFrameKey()
	require.Equal(t, before.stickyWorkHeader, frame.stickyWorkHeader)
	block := m.workHeaderBySource[anchor]
	block.SetExpandedImmediately(true)
	frame = m.transcriptFrameKey()
	require.NotEqual(t, before.stickyWorkGeneration, frame.stickyWorkGeneration)
	require.Contains(t, ansi.Strip(frame.stickyWorkHeader), "Work [-]")
	require.Equal(t, block.HeaderView(), frame.stickyWorkHeader)
}
