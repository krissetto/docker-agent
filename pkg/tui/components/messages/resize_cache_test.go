package messages

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestHeightOnlyResizePreservesTranscriptCachesAndReaderState(t *testing.T) {
	for _, count := range []int{200, 600} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
			t.Cleanup(m.StopAnimations)
			for i := range count {
				view := &countedHistoryView{content: fmt.Sprintf("history %d", i)}
				m.messages = append(m.messages, types.User(view.content))
				m.views = append(m.views, view)
			}
			_ = m.View()
			m.scrollOffset, m.userHasScrolled = 7, true
			m.selection.active = true
			m.selection.startLine, m.selection.endLine = 8, 9
			m.selection.startCol, m.selection.endCol = 1, 3
			_ = m.View()
			selected := m.selection
			lines, offsets := &m.renderedLines[0], &m.lineOffsets[0]
			generation := m.contentGeneration
			rebuilds, misses, renders := m.ResizeCacheStats()
			for _, height := range []int{13, 10, 16, 8, 12} {
				m.SetSize(80, height)
				frame := m.View()
				require.Len(t, strings.Split(frame, "\n"), height)
				for line := range strings.SplitSeq(frame, "\n") {
					require.Equal(t, 80, ansi.StringWidth(line))
				}
				require.Equal(t, 7, m.scrollOffset)
				require.Equal(t, selected, m.selection)
				require.Same(t, lines, &m.renderedLines[0])
				require.Same(t, offsets, &m.lineOffsets[0])
				require.Equal(t, generation, m.contentGeneration)
				r, miss, render := m.ResizeCacheStats()
				require.Equal(t, rebuilds, r)
				require.Equal(t, misses, miss)
				require.Equal(t, renders, render)
			}
			for _, view := range m.views {
				require.Equal(t, 1, view.(*countedHistoryView).renders)
			}
			m.SetSize(60, 12)
			_ = m.View()
			r, miss, render := m.ResizeCacheStats()
			require.Equal(t, rebuilds+1, r)
			require.Equal(t, misses+uint64(count), miss)
			require.Equal(t, renders+uint64(count), render)
		})
	}
}

func TestHeightOnlyResizeFollowsTailAndRendersContentMutations(t *testing.T) {
	m := NewScrollableView(animation.NewRuntime(), 60, 8, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	m.AddUserMessage(strings.Repeat("history\n", 35))
	m.AppendToLastMessage("root", "stable paragraph\n\nactive tail")
	_ = m.View()
	segments := m.activeSegments
	rebuilds, misses, renders := m.ResizeCacheStats()
	for _, height := range []int{5, 14, 8} {
		m.SetSize(60, height)
		_ = m.View()
		require.Equal(t, max(0, m.totalScrollableHeight()-height), m.scrollOffset)
		require.Same(t, segments, m.activeSegments)
	}
	r, miss, render := m.ResizeCacheStats()
	require.Equal(t, rebuilds, r)
	require.Equal(t, misses, miss)
	require.Equal(t, renders, render)
	m.AppendToLastMessage("root", " changed")
	m.SetSize(60, 9)
	require.Contains(t, ansi.Strip(m.View()), "changed")
}

func TestHeightResizeMaterializesNewlyVisibleDeferredContent(t *testing.T) {
	m, chunk := deferredTailFixture(t)
	t.Cleanup(m.StopAnimations)
	m.SetSize(60, 60)
	require.Empty(t, m.deferredTail)
	require.True(t, strings.HasSuffix(m.messages[0].Content, chunk))
	require.Contains(t, ansi.Strip(m.View()), "deferred marker line")
}
