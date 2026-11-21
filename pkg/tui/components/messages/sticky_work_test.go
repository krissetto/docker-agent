package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func stickyFixture(t *testing.T, height int, answer string) *model {
	t.Helper()
	m := newCompactParityModel(t)
	m.SetSize(64, height)
	m.SetPosition(8, 5)
	m.SetCompactWorkLog(true)
	m.AddUserMessage("request")
	m.SetWorkStatus(true, 0)
	m.AppendReasoning("root", "preview one\n\npreview two\n\npreview three")
	if answer != "" {
		m.AppendToLastMessage("root", answer)
	}
	m.View()
	return m
}

func stickyFrameMatchesBody(t *testing.T, m *model, header string, wantInset int) {
	t.Helper()
	frame := strings.Split(ansi.Strip(m.View()), "\n")
	require.Len(t, frame, m.height)
	inset := m.stickyWorkInset()
	require.Equal(t, wantInset, inset)
	if header != "" {
		require.Contains(t, frame[0], header)
	}
	for y := inset; y < m.height; y++ {
		want := strings.TrimSpace(ansi.Strip(m.renderedLine(m.scrollOffset + y - inset)))
		got := strings.TrimSpace(strings.TrimSuffix(frame[y], "│"))
		require.Equal(t, want, got, "row %d", y)
	}
}

func TestStickyWorkInsertedPreviewRowsAndHits(t *testing.T) {
	m := stickyFixture(t, 4, "")
	anchor := m.lineOffsets[1]
	m.userHasScrolled = true
	m.setScrollOffset(anchor + 1)
	before := m.totalHeight
	stickyFrameMatchesBody(t, m, "Working [+]", 1)
	require.Equal(t, before, m.totalHeight, "the header is viewport-only")
	frame := ansi.Strip(m.View())
	for _, line := range []string{"preview one", "preview two", "preview three"} {
		require.Contains(t, frame, line)
	}
	x, y := m.xPos+3, m.yPos
	line, col := m.mouseToLineCol(x, y)
	require.Equal(t, -1, line)
	require.Equal(t, -1, col)
	line, col = m.mouseToLineCol(x, y+1)
	require.Equal(t, anchor+1, line)
	require.Equal(t, 3, col)
	line, col = m.selectionMouseToLineCol(x, y)
	require.Equal(t, anchor+1, line)
	require.Zero(t, col)
	m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	require.Equal(t, anchor, m.scrollOffset)
	require.True(t, m.work.blocks[1].Expanded())
	require.False(t, m.selection.mouseButtonDown)
}

func TestStickyWorkResizeAndBottomReserve(t *testing.T) {
	m := stickyFixture(t, 6, strings.Repeat("answer\n\n", 22))
	m.userHasScrolled = true
	m.setScrollOffset(m.lineOffsets[2] + 7)
	for _, size := range []struct{ w, h int }{{80, 10}, {40, 10}, {64, 4}, {64, 3}, {48, 5}} {
		m.SetSize(size.w, size.h)
		header, inset := "Working [+]", min(5, size.h-1)
		if size.h < 4 {
			header, inset = "", 0
		}
		stickyFrameMatchesBody(t, m, header, inset)
		source, local := m.globalLineToMessageLineCached(m.scrollOffset)
		require.Equal(t, 2, source)
		require.Equal(t, 7, local)
	}
	m.scrollToBottom()
	for _, height := range []int{6, 4, 3, 5} {
		m.SetSize(64, height)
		m.View()
		require.False(t, m.userHasScrolled)
		require.Equal(t, m.totalHeight+m.bottomSlack, m.scrollOffset+m.bodyHeight(), "last canonical row stays reachable")
		require.Equal(t, max(0, m.totalScrollableHeight()-height), m.scrollOffset)
		before := m.scrollOffset
		m.View()
		require.Equal(t, before, m.scrollOffset, "reserve cannot oscillate")
	}
}

func TestStickyWorkResponseFoldPreservesReaderAndPageGeometry(t *testing.T) {
	m := stickyFixture(t, 8, strings.Repeat("answer\n\n", 25))
	m.work.blocks[1].SetExpandedImmediately(true)
	m.invalidateAllItems()
	m.View()
	m.userHasScrolled = true
	start := m.lineOffsets[2] + 4
	m.setScrollOffset(start)
	stickyFrameMatchesBody(t, m, "Working [-]", 1)
	m.scrollPageDown()
	require.Equal(t, start+m.height-1, m.scrollOffset)
	m.scrollPageUp()
	require.Equal(t, start, m.scrollOffset)
	m.handleMouseClick(tea.MouseClickMsg{X: m.xPos + 4, Y: m.yPos, Button: tea.MouseLeft})
	stickyFrameMatchesBody(t, m, "Working [+]", 4)
	idx, local := m.globalLineToMessageLineCached(m.scrollOffset)
	require.Equal(t, 2, idx)
	require.Equal(t, 4, local)
	require.True(t, m.userHasScrolled)
}

func TestStickyWorkReaderStaysOnInertPreviewDuringRepaint(t *testing.T) {
	m := stickyFixture(t, 6, strings.Repeat("answer\n\n", 12))
	m.userHasScrolled = true
	m.setScrollOffset(m.lineOffsets[1] + 1)
	start := m.scrollOffset
	m.renderDirty = true
	stickyFrameMatchesBody(t, m, "Working [+]", 1)
	require.Equal(t, start, m.scrollOffset)
}

func TestStickyWorkKeepsPreviewAfterWheelScroll(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		completed    bool
		preview      []string
	}{
		{name: "working", header: "Working [+]", preview: []string{"preview two", "preview three", "preview four"}},
		{name: "completed", header: "Work [+]", completed: true, preview: []string{"preview one"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newCompactParityModel(t)
			m.SetSize(64, 8)
			m.SetPosition(8, 5)
			m.SetCompactWorkLog(true)
			m.AddUserMessage("request")
			m.SetWorkStatus(true, 0)
			reasoning := "preview one"
			if !tc.completed {
				reasoning += "\n\npreview two\n\npreview three\n\npreview four"
			}
			m.AppendReasoning("root", reasoning)
			m.AppendToLastMessage("root", strings.Repeat("answer\n\n", 20))
			if tc.completed {
				m.SetWorkStatus(false, 0)
			}
			m.View()
			m.scrollToTop()
			_, _ = m.Update(messages.WheelCoalescedMsg{X: m.xPos + 3, Y: m.yPos + 5, Delta: 6})
			require.Greater(t, m.scrollOffset, m.lineOffsets[2], "the original preview has scrolled out of view")

			frame := strings.Split(ansi.Strip(m.View()), "\n")
			require.Len(t, frame, m.height)
			require.Contains(t, frame[0], tc.header)
			for i, line := range tc.preview {
				require.Contains(t, frame[i+1], line)
			}
			wantInset := 1 + len(tc.preview)
			if !blankTranscriptLine(m.renderedLine(m.scrollOffset)) {
				wantInset++
			}
			stickyFrameMatchesBody(t, m, tc.header, wantInset)
			require.Empty(t, stickyFrameContent(frame[1+len(tc.preview)]), "one blank row separates pinned preview from body")
			line, col := m.mouseToLineCol(m.xPos+3, m.yPos+1)
			require.Equal(t, -1, line, "the pinned preview is not a transcript hit")
			require.Equal(t, -1, col)
			_, _ = m.Update(tea.MouseClickMsg{X: m.xPos + 3, Y: m.yPos + 1, Button: tea.MouseLeft})
			require.False(t, m.work.blocks[1].Expanded(), "only the pinned header toggles Work")
			require.False(t, m.selection.mouseButtonDown)
		})
	}
}

func stickyFrameContent(line string) string {
	return strings.TrimSpace(strings.TrimSuffix(line, "│"))
}

func TestCompactWorkPreviewGapBeforeReply(t *testing.T) {
	for _, sender := range []string{"root", "other"} {
		t.Run(sender, func(t *testing.T) {
			m := stickyFixture(t, 20, "")
			m.AppendToLastMessage(sender, "reply text")
			m.scrollToTop()
			frame := strings.Split(ansi.Strip(m.View()), "\n")
			workEnd := m.work.ends[1]
			require.Contains(t, frame[workEnd-1], "preview three")
			require.Empty(t, stickyFrameContent(frame[workEnd]))
			require.NotEmpty(t, stickyFrameContent(frame[workEnd+1]), "no second boundary gap")
			x, y := m.xPos+3, m.yPos+workEnd
			if sender == "root" {
				line, col := m.mouseToLineCol(x, y)
				require.Equal(t, workEnd, line, "the blank action row remains a source row")
				require.Equal(t, 3, col)
				m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y})
				m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y})
				frame = strings.Split(ansi.Strip(m.View()), "\n")
				require.Empty(t, stickyFrameContent(frame[workEnd]))
				require.Contains(t, frame[workEnd+1], types.MessageCopyLabel)
				m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y + 1})
				labelCol := strings.Index(frame[workEnd+1], types.MessageCopyLabel)
				require.Positive(t, labelCol)
				require.True(t, m.isCopyLabelClick(2, 0, labelCol))
			} else {
				require.Contains(t, frame[workEnd+1], sender)
				m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y})
				require.Equal(t, -1, m.hoveredMessageIndex, "spacer cannot start reply hover")
			}
			line, col := m.mouseToLineCol(x, y)
			require.Equal(t, -1, line)
			require.Equal(t, -1, col)
			m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.False(t, m.work.blocks[1].Expanded())
			require.False(t, m.selection.mouseButtonDown)
			line, col = m.mouseToLineCol(x, y+1)
			require.Equal(t, m.lineOffsets[2], line)
			require.Equal(t, 3, col)
			idx, local := m.globalLineToMessageLineCached(line)
			require.Equal(t, 2, idx)
			require.Zero(t, local)
		})
	}
}

func TestStickyWorkPreviewGapBodyMapping(t *testing.T) {
	m := stickyFixture(t, 8, strings.Repeat("answer\n\n", 22))
	m.userHasScrolled = true
	for _, tc := range []struct {
		local, inset int
		synthetic    bool
	}{{0, 4, false}, {1, 5, true}} {
		start := m.lineOffsets[2] + tc.local
		m.setScrollOffset(start)
		before := m.totalHeight
		stickyFrameMatchesBody(t, m, "Working [+]", tc.inset)
		frame := strings.Split(ansi.Strip(m.View()), "\n")
		require.Contains(t, frame[3], "preview three")
		require.Empty(t, stickyFrameContent(frame[4]))
		require.Contains(t, frame[5], "answer")
		require.Equal(t, before, m.totalHeight)
		require.Equal(t, start, m.scrollOffset)
		x, y := m.xPos+3, m.yPos+4
		line, col := m.mouseToLineCol(x, y)
		if tc.synthetic {
			require.Equal(t, -1, line)
			require.Equal(t, -1, col)
			m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			require.False(t, m.work.blocks[1].Expanded())
			require.False(t, m.selection.mouseButtonDown)
		} else {
			require.Equal(t, start, line, "a reused row keeps its original mapping")
			require.Equal(t, 3, col)
		}
		line, col = m.selectionMouseToLineCol(x, y+1)
		require.Equal(t, m.lineOffsets[2]+1, line)
		require.Equal(t, 3, col)
	}
}
