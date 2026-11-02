package messages

import (
	"fmt"
	"math/bits"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// budgetRenders sums body renders over every view. Test-side only: it reads the
// fixtures' own counters, not the production work counters.
func budgetRenders(m *model) uint64 {
	var n uint64
	for _, view := range m.views {
		n += uint64(view.(*countedHistoryView).renders)
	}
	return n
}

// TestLayoutBudget6000ItemsStaysViewportBounded checks the lazy layout on a
// 6000-item transcript. The first View is the initial load and may measure the
// whole history once. After that, the total accessor does index work only, and
// width resize, theme change and a one-message append render, measure and place
// only what the viewport shows, at the top, the middle and the bottom.
func TestLayoutBudget6000ItemsStaysViewportBounded(t *testing.T) {
	const items, height = 6000, 24
	// Each one-line item plus its separator is two rows, so the viewport shows
	// about height/2 items; allow the partly visible edges and the streaming tail.
	viewportBound := uint64(height/2 + 6)
	for _, pos := range []string{"top", "middle", "bottom"} {
		t.Run(pos, func(t *testing.T) {
			m := NewScrollableView(animation.NewRuntime(), 80, height, &service.SessionState{}).(*model)
			t.Cleanup(m.StopAnimations)
			for i := range items {
				view := &countedHistoryView{content: fmt.Sprintf("message %d", i)}
				m.messages = append(m.messages, types.User(view.content))
				m.views = append(m.views, view)
			}
			_ = m.View()
			// Initial load renders only what the viewport shows and fills; every
			// other item stays a placeholder until it is shown.
			require.LessOrEqual(t, budgetRenders(m), viewportBound, "initial load renders only the viewport and fill")
			switch pos {
			case "top":
				m.userHasScrolled = true
				m.setScrollOffset(0)
			case "middle":
				m.userHasScrolled = true
				m.setScrollOffset(m.totalHeight / 2)
			default:
				_ = m.scrollToBottom()
			}
			_ = m.View()

			before := m.layoutWorkStats()
			renders := budgetRenders(m)
			total := m.RenderedContentHeight()
			after := m.layoutWorkStats()
			require.Equal(t, renders, budgetRenders(m), "RenderedContentHeight renders no body")
			require.Equal(t, before.itemVisits, after.itemVisits, "RenderedContentHeight measures no slot")
			require.LessOrEqual(t, after.indexNodes-before.indexNodes, uint64(2*bits.Len(items)), "RenderedContentHeight is O(log N) index work")
			require.Equal(t, m.totalHeight, total)

			step := func(name string, act func()) {
				t.Helper()
				before := m.layoutWorkStats()
				renders := budgetRenders(m)
				act()
				_ = m.View()
				after := m.layoutWorkStats()
				require.LessOrEqual(t, budgetRenders(m)-renders, viewportBound, "%s renders only viewport-bounded items", name)
				require.LessOrEqual(t, after.itemVisits-before.itemVisits, viewportBound, "%s measures only viewport-bounded slots", name)
				require.LessOrEqual(t, after.facadeEntries-before.facadeEntries, 4*viewportBound, "%s places only viewport-bounded rows", name)
				require.Equal(t, before.slotRebuilds, after.slotRebuilds, "%s needs no full slot index rebuild", name)
				require.LessOrEqual(t, after.slotLoops-before.slotLoops, 4*viewportBound, "%s runs no N-proportional slot loop", name)
				// The retention window is the viewport plus one screen on each side, so
				// the layout visits up to three screens of slots, each O(log N).
				require.LessOrEqual(t, after.indexNodes-before.indexNodes, 12*viewportBound*uint64(bits.Len(items)), "%s index work is window slots times O(log N)", name)
				require.LessOrEqual(t, after.sizeCalls-before.sizeCalls, 4*viewportBound, "%s sizes only viewport-bounded views", name)
				require.LessOrEqual(t, after.projectionVisits-before.projectionVisits, 4*viewportBound, "%s visits no global projection", name)
				t.Logf("%s/%s: renders+%d itemVisits+%d facadeEntries+%d slotLoops+%d indexNodes+%d slotRebuilds+%d",
					pos, name, budgetRenders(m)-renders, after.itemVisits-before.itemVisits, after.facadeEntries-before.facadeEntries,
					after.slotLoops-before.slotLoops, after.indexNodes-before.indexNodes, after.slotRebuilds-before.slotRebuilds)
			}
			step("width-resize", func() { m.SetSize(60, height) })
			step("theme", func() { _, _ = m.Update(messages.ThemeChangedMsg{}) })
			step("append-one", func() {
				view := &countedHistoryView{content: fmt.Sprintf("message %d", items)}
				m.messages = append(m.messages, types.User(view.content))
				m.views = append(m.views, view)
			})
		})
	}
}

// visibleSlotCount counts the slots whose rows intersect the current viewport.
func (m *model) visibleSlotCount() int {
	if len(m.views) == 0 {
		return 0
	}
	first := m.slotAt(m.scrollOffset)
	last := m.slotAt(max(m.scrollOffset, m.scrollOffset+m.height-1))
	return min(last, len(m.views)-1) - first + 1
}

// TestOffscreenTailIsNotRenderedByEpochsWithoutStream: the tail is measured only
// while it streams. With the reader above the end and nothing streaming, a width
// or theme epoch renders no body for it.
func TestOffscreenTailIsNotRenderedByEpochsWithoutStream(t *testing.T) {
	const items, height = 200, 24
	m := NewScrollableView(animation.NewRuntime(), 80, height, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	for i := range items {
		view := &countedHistoryView{content: fmt.Sprintf("message %d", i)}
		m.messages = append(m.messages, types.User(view.content))
		m.views = append(m.views, view)
	}
	_ = m.View()
	m.userHasScrolled = true
	m.setScrollOffset(0)
	_ = m.View()
	tail := m.views[items-1].(*countedHistoryView)
	renders := tail.renders
	m.SetSize(60, height)
	_ = m.View()
	_, _ = m.Update(messages.ThemeChangedMsg{})
	_ = m.View()
	require.Equal(t, renders, tail.renders, "an offscreen tail that is not streaming renders no body on width or theme epochs")
	require.False(t, m.materialized(items-1))
}

// TestOffscreenLiveTailMeasuresOnlyOnContentChange: with a stream active and the
// reader scrolled up, width and theme epochs measure only the viewport and leave
// the live tail unrendered. The next chunk to that tail measures it exactly once.
func TestOffscreenLiveTailMeasuresOnlyOnContentChange(t *testing.T) {
	const items, height = 200, 24
	viewportBound := uint64(height/2 + 6)
	m := NewScrollableView(animation.NewRuntime(), 80, height, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	for i := range items {
		view := &countedHistoryView{content: fmt.Sprintf("message %d", i)}
		m.messages = append(m.messages, types.User(view.content))
		m.views = append(m.views, view)
	}
	_ = m.View()
	m.AppendToLastMessage("root", "streamed words streamed words streamed words streamed words")
	_ = m.View()
	tail := len(m.views) - 1
	require.NotNil(t, m.activeSegments, "the live stream builds its segmented suffix")
	m.userHasScrolled = true
	m.setScrollOffset(0)
	_ = m.View()

	before := m.stats
	m.SetSize(60, height)
	_ = m.View()
	require.LessOrEqual(t, m.stats.itemVisits-before.itemVisits, viewportBound, "a width epoch measures only viewport-bounded slots")
	require.False(t, m.materialized(tail), "a width epoch leaves the offscreen live tail unrendered")

	before = m.stats
	_, _ = m.Update(messages.ThemeChangedMsg{})
	_ = m.View()
	require.LessOrEqual(t, m.stats.itemVisits-before.itemVisits, viewportBound, "a theme epoch measures only viewport-bounded slots")
	require.False(t, m.materialized(tail), "a theme epoch leaves the offscreen live tail unrendered")

	before = m.stats
	m.AppendToLastMessage("root", " next chunk")
	_ = m.View()
	require.Equal(t, uint64(1), m.stats.itemVisits-before.itemVisits, "the next chunk measures the live tail exactly once")
	_ = m.View()
	require.Equal(t, uint64(1), m.stats.itemVisits-before.itemVisits, "the settled tail is not measured again by later Views")
}
