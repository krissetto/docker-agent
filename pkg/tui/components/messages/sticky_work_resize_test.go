package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/types"
)

// stickyFrameMatchesBody checks the inserted header row and that every body
// row paints the canonical transcript row starting at scrollOffset.
func stickyFrameMatchesBody(t *testing.T, m *model, header string) {
	t.Helper()
	frame := strings.Split(ansi.Strip(m.View()), "\n")
	require.Len(t, frame, m.height)
	inset := m.stickyWorkInset()
	if header != "" {
		require.Equal(t, 1, inset)
		require.Contains(t, frame[0], header)
	} else {
		require.Zero(t, inset)
	}
	for y := inset; y < m.height && m.scrollOffset+y-inset < m.bodyScrollableHeight(); y++ {
		want := strings.TrimSpace(ansi.Strip(m.renderedLine(m.scrollOffset + y - inset)))
		require.Equal(t, want, strings.TrimSpace(strings.TrimSuffix(frame[y], "│")), "row %d", y)
	}
	require.LessOrEqual(t, m.scrollOffset, max(0, m.totalScrollableHeight()-m.height), "clamped")
}

// Resizing while the inserted sticky row is visible keeps a scrolled reader on
// the same source row, keeps the canonical hit mapping one row below the
// header, and drops/restores the inset across the four-row threshold.
func TestStickyWorkResizeWhileInsertedHeaderVisible(t *testing.T) {
	m, anchors := stickyWorkHistory(t)
	answer := anchors[1] + 1
	m.setScrollOffset(m.slotOffset(answer) + 6)
	m.View()
	require.Equal(t, 1, m.stickyWorkInset())
	source, local := m.globalLineToMessageLineCanonical(m.scrollOffset)
	require.Equal(t, answer, source)

	for _, size := range []struct{ w, h int }{{80, 10}, {40, 10}, {64, 4}, {64, 3}, {48, 3}, {48, 5}, {64, 10}} {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			m.SetSize(size.w, size.h)
			m.View()
			require.True(t, m.userHasScrolled)
			gotSource, gotLocal := m.globalLineToMessageLineCanonical(m.scrollOffset)
			require.Equal(t, source, gotSource, "reader stays on its source")
			require.Equal(t, local, gotLocal, "reader stays on its row")
			if size.h < 4 {
				stickyFrameMatchesBody(t, m, "")
				_, _, ok := m.stickyWorkAt(m.xPos+3, m.yPos)
				require.False(t, ok)
				line, _ := m.mouseToLineCol(m.xPos+3, m.yPos)
				require.Equal(t, m.scrollOffset, line)
				return
			}
			stickyFrameMatchesBody(t, m, "Work [+]")
			_, block, ok := m.stickyWorkAt(m.xPos+3, m.yPos)
			require.True(t, ok)
			require.Same(t, m.workHeaderBySource[anchors[1]], block)
			line, col := m.mouseToLineCol(m.xPos+3, m.yPos+1)
			require.Equal(t, m.scrollOffset, line)
			require.Equal(t, 3, col)
			line, _ = m.mouseToLineCol(m.xPos+3, m.yPos+size.h-1)
			require.Equal(t, m.scrollOffset+size.h-2, line)
			line, _ = m.selectionMouseToLineCol(m.xPos+3, m.yPos)
			require.Equal(t, m.scrollOffset, line)
		})
	}
}

// A bottom-pinned reader keeps the true last row reachable through resizes:
// the reserve follows the sticky condition at each size and the clamp includes it.
func TestStickyWorkResizeKeepsBottomPinnedReserve(t *testing.T) {
	m, _ := stickyPreviewModel(t, 6, strings.Repeat("answer\n\n", 12))
	m.scrollToBottom()
	m.View()
	for _, size := range []struct{ w, h int }{{64, 6}, {80, 6}, {40, 6}, {40, 4}, {40, 3}, {64, 3}, {64, 5}, {64, 6}} {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			m.SetSize(size.w, size.h)
			m.View()
			require.False(t, m.userHasScrolled)
			require.Equal(t, max(0, m.totalScrollableHeight()-size.h), m.scrollOffset, "pinned to clamp")
			require.Equal(t, m.bodyScrollableHeight(), m.scrollOffset+m.bodyHeight(), "last canonical row is the last painted body row")
			if size.h < 4 {
				require.Zero(t, m.stickyWorkInset())
				require.Zero(t, m.totalScrollableHeight()-m.bodyScrollableHeight(), "no reserve below four rows")
				stickyFrameMatchesBody(t, m, "")
			} else {
				require.Equal(t, 1, m.stickyWorkInset())
				require.Equal(t, 1, m.totalScrollableHeight()-m.bodyScrollableHeight(), "reserve while sticky at bottom")
				stickyFrameMatchesBody(t, m, "Working [+]")
			}
		})
	}
}

// Streaming growth of exactly one canonical row keeps a bottom-pinned reader
// pinned: the follow baseline is the reserve-free body total. A single
// paragraph grows by one word per delta, so each wrap adds exactly one row.
func TestStickyWorkOneRowGrowthStaysPinnedWithReserve(t *testing.T) {
	for _, height := range []int{4, 6} {
		t.Run(fmt.Sprint(height), func(t *testing.T) {
			m, anchor := stickyPreviewModel(t, height, "")
			words := []string{"w0"}
			m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Join(words, " ")))
			last := len(m.messages) - 1
			require.Equal(t, anchor+1, last)
			m.scrollToBottom()
			m.View()
			oneRowSteps, stickySteps := 0, 0
			for i := 1; oneRowSteps < 8; i++ {
				require.Less(t, i, 400, "paragraph never wrapped")
				prevBody := m.bodyScrollableHeight()
				require.Equal(t, m.followTotal+m.bottomSlack+m.scrollOnSend.padding, prevBody, "follow baseline excludes the reserve")
				words = append(words, fmt.Sprintf("w%d", i))
				m.messages[last].Content = strings.Join(words, " ")
				m.views[last] = m.createMessageView(m.messages[last])
				m.invalidateItem(last)
				m.View()
				delta := m.bodyScrollableHeight() - prevBody
				require.Contains(t, []int{0, 1}, delta, "one word adds at most one row")
				require.False(t, m.userHasScrolled)
				require.Equal(t, max(0, m.totalScrollableHeight()-height), m.scrollOffset, "pinned after +%d row", delta)
				if m.bodyScrollableHeight() > height {
					require.Equal(t, m.bodyScrollableHeight(), m.scrollOffset+m.bodyHeight(), "last canonical row painted")
				}
				require.Contains(t, ansi.Strip(m.View()), fmt.Sprintf("w%d", i), "newest word painted")
				if delta == 1 {
					oneRowSteps++
					if m.stickyWorkInset() == 1 {
						stickySteps++
						require.Equal(t, 1, m.totalScrollableHeight()-m.bodyScrollableHeight(), "reserve present")
					}
				}
			}
			require.Positive(t, stickySteps, "exercised one-row growth with the sticky reserve active")
		})
	}
}
