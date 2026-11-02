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
			// APPROVED ADAPTATION (branch-only). A height-only change keeps every
			// still-valid visible entry: those slots must not render again. Only a
			// slot that the new viewport exposes for the first time may render, and
			// the count is bounded by the newly exposed visible slots.
			newlyExposed := make([]int, len(m.views))
			// The reader's first frame before the loop is an exposure too.
			for _, i := range identityVisible(m) {
				newlyExposed[i]++
			}
			for _, height := range []int{13, 10, 16, 8, 12} {
				prevVisible := identityVisible(m)
				perViewBefore := countedRenders(m)
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
				exposed := 0
				for _, i := range identityVisible(m) {
					if !containsSlot(prevVisible, i) {
						exposed++
						newlyExposed[i]++
					}
				}
				for i, after := range countedRenders(m) {
					if after != perViewBefore[i] {
						require.False(t, containsSlot(prevVisible, i), "height %d re-rendered slot %d, which was visible before and is still valid", height, i)
					}
				}
				require.LessOrEqual(t, render-renders, uint64(exposed), "a height-only change renders only newly exposed visible slots")
				require.LessOrEqual(t, miss-misses, uint64(exposed), "cache misses follow the newly exposed renders only")
				renders, misses = render, miss
			}
			// A slot renders once when it is first shown and once more each time a
			// height change exposes it again after the viewport released it.
			for i, view := range m.views {
				require.LessOrEqual(t, view.(*countedHistoryView).renders, 1+newlyExposed[i], "slot %d renders only when newly exposed", i)
			}
			shownBefore := identityVisible(m)
			m.SetSize(60, 12)
			_ = m.View()
			r, miss, render := m.ResizeCacheStats()
			require.Equal(t, rebuilds+1, r)
			// Old assertion required every item to re-render on this width change (an eager layout).
			// The lazy layout renders only the items this width shows, plus the items shown
			// before the clamp that the settling pass had to measure: the bound is the union
			// of the slots visible before and after the change.
			bound := uint64(len(identityUnion(shownBefore, identityVisible(m))))
			require.LessOrEqual(t, miss-misses, bound, "a shrink misses only items in the before/after viewports")
			require.LessOrEqual(t, render-renders, bound, "a shrink renders only items in the before/after viewports")
			require.Less(t, bound, uint64(count), "the viewports are a small part of the transcript")
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

// countedRenders returns the body renders of each countedHistoryView.
func countedRenders(m *model) []int {
	out := make([]int, len(m.views))
	for i, v := range m.views {
		out[i] = v.(*countedHistoryView).renders
	}
	return out
}
