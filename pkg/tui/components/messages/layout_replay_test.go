package messages

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// replayMaterializedSlots lists the slots that hold a body.
func replayMaterializedSlots(m *model) []int {
	seen := map[int]bool{}
	for i := range m.renderedItems {
		seen[i] = true
	}
	for i := range m.uncached {
		seen[i] = true
	}
	var out []int
	for i := range seen {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// TestReplayLoadBodyWorkStaysViewportBounded loads a whole history through the
// replay path (BeginReplay, recent-first drain, first frame, older-history paging,
// final frame) without resetting counters between phases, for the mixed and heavy
// fixtures. Body work is counted per category: synchronous renders (renderItem, which
// includes the message's own markdown), composite bodies, prepares scheduled,
// executed on a worker, and applied, and reasoning prepares. renderItem schedules no
// prepare, so a message prepare shows up in both syncRenders (the body it reuses) and
// prepareExecuted; each category is bounded separately, never summed.
//
// The bound is the viewport union: the slots whose rows intersect the first or the
// final viewport. Metadata visits (itemVisits, sizeCalls) are reported, not bounded.
func TestReplayLoadBodyWorkStaysViewportBounded(t *testing.T) {
	for _, fx := range vprlFixtures() {
		for _, n := range []int{100, 1000, 6000} {
			t.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(t *testing.T) {
				sess := fx.build(t, n)
				// An oversized assistant answer in the middle of history is offscreen for the
				// whole load, so no prepare may be scheduled for it.
				sess.Messages[(n/2)|1].Message.Message.Content = strings.Repeat("word ", 4000)
				m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
				t.Cleanup(m.StopAnimations)
				BeginReplay(m, PrepareReplay(sess), nil)
				drainRetainedReplay(t, m)
				afterDrain := m.layoutWorkStats()

				first := m.View()
				require.NotEmpty(t, strings.TrimSpace(ansi.Strip(first)), "the first usable frame must show transcript content")
				union := map[int]bool{}
				for _, i := range vprlViewportSlots(m) {
					union[i] = true
				}

				finishHistoryReplay(t, m)
				afterFinish := m.layoutWorkStats()
				final := m.View()
				require.NotEmpty(t, strings.TrimSpace(ansi.Strip(final)), "the final frame must show transcript content")
				// Paging older history adds only estimates above the reader; the bottom rows
				// must stay on the same screen rows.
				require.Equal(t, ansi.Strip(first), ansi.Strip(final), "older-history paging must not move the visible rows")
				for _, i := range vprlViewportSlots(m) {
					union[i] = true
				}

				s := m.layoutWorkStats()
				bound := uint64(len(union))
				t.Logf("fixture=%s messages=%d union=%d drain{sync=%d prepSched=%d} finish{sync=%d} total{sync=%d composite=%d prepare=%d/%d/%d reasoning=%d/%d/%d} itemVisits=%d sizeCalls=%d materialized=%v",
					fx.name, n, len(union), afterDrain.syncRenders, afterDrain.prepareScheduled, afterFinish.syncRenders,
					s.syncRenders, s.compositeViews,
					s.prepareScheduled, s.prepareExecuted, s.prepareApplied,
					s.reasoningScheduled, s.reasoningExecuted, s.reasoningApplied,
					s.itemVisits, s.sizeCalls, replayMaterializedSlots(m))

				// Every body category stays inside the viewport union, independently.
				require.LessOrEqual(t, s.syncRenders, bound, "synchronous renders are bounded by the viewport")
				require.LessOrEqual(t, s.compositeViews, bound, "composite bodies are bounded by the viewport")
				require.LessOrEqual(t, s.prepareScheduled, bound, "prepares are scheduled only for the viewport")
				require.LessOrEqual(t, s.prepareExecuted, bound, "prepares execute only for the viewport")
				require.LessOrEqual(t, s.reasoningScheduled, bound, "reasoning prepares are scheduled only for the viewport")
				require.LessOrEqual(t, s.reasoningExecuted, bound, "reasoning prepares execute only for the viewport")
				require.LessOrEqual(t, s.prepareExecuted, s.prepareScheduled, "a prepare cannot execute unless it was scheduled")
				require.LessOrEqual(t, s.prepareApplied, s.prepareExecuted, "a prepare cannot apply unless it executed")
				require.Zero(t, s.prepareScheduled, "an offscreen oversized answer must not prepare during load")
				require.Zero(t, s.reasoningScheduled, "no offscreen reasoning prepare during load")
				for _, i := range replayMaterializedSlots(m) {
					require.True(t, union[i], "slot %d holds a body outside the viewport union", i)
				}
			})
		}
	}
}

// TestReplayResizeAndThemeAfterPagingStayViewportBounded checks that once every
// history item is paged in, a width change and a theme change re-render only the
// covering tail. Each prepares only the visible oversized item, and the offscreen one
// is never prepared.
func TestReplayResizeAndThemeAfterPagingStayViewportBounded(t *testing.T) {
	sess := historyReplayFixture(40)
	sess.Messages[5] = assistantItem("worker", strings.Repeat("offscreen oversized\n\n", 1500), "")
	sess.Messages[39] = assistantItem("worker", strings.Repeat("visible oversized\n\n", 1500), "")
	m := NewScrollableView(animation.NewRuntime(), 80, 12, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	BeginReplay(m, PrepareReplay(sess), nil)
	finishPreparedReplay(t, m)
	finishHistoryReplay(t, m)
	require.Zero(t, m.historyStart, "all history is paged in")
	base := m.layoutWorkStats()
	require.Equal(t, uint64(1), base.prepareScheduled, "the load prepares only the visible oversized tail")

	m.SetSize(70, 12)
	require.True(t, ReconcileReplay(m), "a width change re-renders the transcript")
	finishPreparedReplay(t, m)
	afterResize := m.layoutWorkStats()
	require.Equal(t, base.prepareScheduled+1, afterResize.prepareScheduled, "a resize prepares only the visible oversized item")
	require.LessOrEqual(t, afterResize.syncRenders-base.syncRenders, uint64(1), "a resize renders only the covering tail")

	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	styles.ApplyTheme(styles.DefaultTheme())
	require.True(t, ReconcileReplay(m), "a theme change re-renders the transcript")
	finishPreparedReplay(t, m)
	afterTheme := m.layoutWorkStats()
	require.Equal(t, afterResize.prepareScheduled+1, afterTheme.prepareScheduled, "a theme change prepares only the visible oversized item")
	require.LessOrEqual(t, afterTheme.syncRenders-afterResize.syncRenders, uint64(1), "a theme change renders only the covering tail")
	require.Zero(t, afterTheme.prepareScheduled-afterResize.prepareScheduled-1, "the offscreen item is never prepared")
}
