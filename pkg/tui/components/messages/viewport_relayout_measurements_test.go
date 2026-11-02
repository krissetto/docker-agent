package messages

// V3 — NOT APPLIED, NOT COMPILED. Target when approved: a NEW file
// pkg/tui/components/messages/viewport_relayout_measurements_test.go in the fork.
// Uses fork-only APIs (layoutWorkStats, SetCompactWorkLog, ReplyHandoffActive, replyHandoff.Rows);
// the parent cannot compile this file.

import (
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// ---- A. compact-work toggle: timed toggle call only; first View timed manually ----

// BenchmarkViewportRelayoutCompactToggle times ONLY SetCompactWorkLog per op. The first
// View after the toggle is timed manually while the framework timer is stopped.
// Counters are read outside the timer. Per op: slotLoops, slotRebuilds, itemVisits,
// sizeCalls and renders are deltas over the toggle plus that View.
func BenchmarkViewportRelayoutCompactToggle(b *testing.B) {
	for _, n := range vprlSizes {
		for _, from := range []bool{true, false} {
			name := fmt.Sprintf("from=%s/messages=%d", map[bool]string{true: "on", false: "off"}[from], n)
			b.Run(name, func(b *testing.B) {
				m := buildWorkStatusSessionHistory(b, from, n/2)
				_ = m.View()
				var toggleTotal, viewTotal time.Duration
				var slotLoops, rebuilds, visits, sizeCalls, renders uint64
				var visibleMax int
				on := from
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					b.StopTimer()
					on = !on // every op is a real toggle in the opposite direction
					enable := on
					s0 := m.layoutWorkStats()
					r0 := vprlRenders(m)
					b.StartTimer()
					t0 := time.Now()
					_ = m.SetCompactWorkLog(enable)
					toggleTotal += time.Since(t0)
					b.StopTimer()
					t1 := time.Now()
					_ = m.View()
					viewTotal += time.Since(t1)
					s1 := m.layoutWorkStats()
					r1 := vprlRenders(m)
					slotLoops += s1.slotLoops - s0.slotLoops
					rebuilds += s1.slotRebuilds - s0.slotRebuilds
					visits += s1.itemVisits - s0.itemVisits
					sizeCalls += s1.sizeCalls - s0.sizeCalls
					renders += r1 - r0
					if v := m.visibleSlotCount(); v > visibleMax {
						visibleMax = v
					}
					// Scope proof: a toggle may size only the visible slots, plus the anchor slack.
					if sizeCalls > uint64(b.N*(visibleMax+vprlAnchorSlack)) {
						b.Fatalf("toggle issued %d SetSize calls, above visible+slack %d", sizeCalls, visibleMax+vprlAnchorSlack)
					}
					if rebuilds > uint64(b.N) {
						b.Fatalf("toggle rebuilt the slot index %d times over %d ops, more than one pass per toggle", rebuilds, b.N)
					}
				}
				b.StopTimer()
				ops := float64(b.N)
				b.ReportMetric(float64(toggleTotal.Nanoseconds())/ops, "ns/toggle")
				b.ReportMetric(float64(viewTotal.Nanoseconds())/ops, "ns/view")
				b.ReportMetric(float64(slotLoops)/ops, "slotLoops/op")
				b.ReportMetric(float64(rebuilds)/ops, "slotRebuilds/op")
				b.ReportMetric(float64(visits)/ops, "itemVisits/op")
				b.ReportMetric(float64(sizeCalls)/ops, "sizeCalls/op")
				b.ReportMetric(float64(renders)/ops, "renderedMessages/op")
			})
		}
	}
}

// ---- B. repeated scroll + real reply handoff, retention window, cache owners ----

// vprlSlotsInRows returns the slot indices whose rows intersect [top, bottom). Same exact
// displayEnd search as vprlVisibleItems and vprlViewportSlots; it reads no lineOffsets.
func vprlSlotsInRows(m *model, top, bottom int) []int {
	n := len(m.views)
	if n == 0 || bottom <= top {
		return nil
	}
	start := func(i int) int {
		if i == 0 {
			return 0
		}
		return m.displayEnd(i - 1)
	}
	first := sortSearch(n, func(i int) bool { return m.displayEnd(i) > top })
	past := sortSearch(n, func(i int) bool { return start(i) >= bottom })
	var out []int
	for i := first; i < past; i++ {
		if m.displayEnd(i) > start(i) {
			out = append(out, i)
		}
	}
	return out
}

// sortSearch is sort.Search, kept local so the file imports only what it uses.
func sortSearch(n int, f func(int) bool) int {
	lo, hi := 0, n
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if !f(mid) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// vprlScrollToRow moves the reader to target with real WheelCoalescedMsg input. One notch
// moves wheelScrollAmount*defaultScrollAmount rows (messages.go), and the reader is
// re-read after each View, so estimated heights settle before the final check.
func vprlScrollToRow(t *testing.T, m *model, target int) {
	t.Helper()
	const rowsPerNotch = wheelScrollAmount * defaultScrollAmount
	for range 2 {
		delta := (target - m.scrollOffset) / rowsPerNotch
		if delta != 0 {
			_, _ = m.Update(messages.WheelCoalescedMsg{Delta: delta, X: 5, Y: 5})
		}
		_ = m.View()
	}
	require.LessOrEqualf(t, absInt(m.scrollOffset-target), rowsPerNotch, "reader must land within one notch of row %d, got %d", target, m.scrollOffset)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// vprlKeys returns the materialized slot IDs of one cache, sorted, for the owner log.
func vprlKeys(idx renderedItemIndex) []int {
	keys := make([]int, 0, len(idx))
	for k := range idx {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// vprlDistinctSources counts the distinct source slots a handoff's rows reference.
func vprlDistinctSources(rows []replyHandoffFrameRow) int {
	seen := map[int]bool{}
	for _, r := range rows {
		seen[r.SourceIndex] = true
	}
	return len(seen)
}

// vprlRetentionWindow is the geometric window the policy allows: the viewport plus one
// screen above and one screen below (rows [scroll-height, scroll+2*height)), plus the
// slots the reply handoff holds, plus the ACTIVE streaming tail only while it is streaming
// (m.activeSegments is non-nil). A completed, offscreen tail is not in the policy.
func vprlRetentionWindow(m *model) (window, handoff map[int]bool, tail int) {
	top := max(0, m.scrollOffset-m.height)
	bottom := m.scrollOffset + 2*m.height
	window = map[int]bool{}
	for _, i := range vprlSlotsInRows(m, top, bottom) {
		window[i] = true
	}
	// Idle Rows() returns the last captured exchange layout, which the policy does not
	// retain. Handoff sources count only while a handoff is active.
	handoff = map[int]bool{}
	if m.ReplyHandoffActive() {
		for _, r := range m.replyHandoff.Rows() {
			handoff[r.SourceIndex] = true
		}
	}
	tail = -1
	if m.activeSegments != nil {
		tail = m.activeSegments.index
	}
	return window, handoff, tail
}

// vprlAssertRetention checks every materialized key, cached or uncached, against the
// policy set (window union handoff union active tail). A key outside the transcript is
// malformed. Keys are deduplicated across both caches, and the distinct count must not
// exceed the size of the policy set. This is an exact subset check, not a count slack.
func vprlAssertRetention(t *testing.T, m *model, where string) {
	t.Helper()
	window, handoff, tail := vprlRetentionWindow(m)
	policy := map[int]bool{}
	for i := range window {
		policy[i] = true
	}
	for i := range handoff {
		policy[i] = true
	}
	if tail >= 0 {
		policy[tail] = true
	}
	seen := map[int]bool{}
	check := func(kind string, key int) {
		require.Truef(t, key >= 0 && key < len(m.views), "%s: %s key %d is malformed for %d views", where, kind, key, len(m.views))
		require.Truef(t, policy[key], "%s: %s body %d lies outside the retention policy", where, kind, key)
		seen[key] = true
	}
	for i := range m.renderedItems {
		check("cached", i)
	}
	for i := range m.uncached {
		check("uncached", i)
	}
	require.LessOrEqualf(t, len(seen), len(policy), "%s: %d distinct bodies exceed the policy set %d", where, len(seen), len(policy))
}

// vprlHandoffOnce starts one real reply handoff through the production append path, using
// the fourth-reply fixture sequence, and retires it through accepted ticks.
func vprlHandoffOnce(t *testing.T, m *model, cycle int) (peakRows, peakSources int) {
	t.Helper()
	m.suppressReplyHandoff = true
	for i := range 3 {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", fmt.Sprintf("v4 seed %d-%d prose", cycle, i)))
		m.committedMessageCount = len(m.messages)
	}
	m.suppressReplyHandoff = false
	_ = m.View()
	call := tools.ToolCall{ID: fmt.Sprintf("v3-call-%d", cycle), Function: tools.FunctionCall{Name: "check"}}
	_ = m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	_ = m.AddAssistantMessage("root", "")
	_ = m.AppendAssistantMedia("root", []types.AssistantMedia{{Fallback: fmt.Sprintf("v3 media %d", cycle)}})
	_ = m.AddAssistantMessage("root", "")
	_ = m.View()
	require.False(t, m.renderDirty, "precondition: rendered before the first prose chunk")
	_ = m.AppendToLastMessage("root", fmt.Sprintf("v3 prose %d", cycle))
	require.True(t, m.ReplyHandoffActive(), "the first prose chunk must start a reply handoff")
	// The phase depends on the frame at the trigger: the sequence starts in fade-out when
	// the oldest visible reply retires, and the batch can already be in reflow or fade-in
	// after the first frame. A real retiring reply and handoff rows are required, so an
	// incoming-only handoff cannot pass.
	phase := m.replyHandoff.Phase()
	require.Truef(t, phase == replyHandoffFadeOut || phase == replyHandoffReflow || phase == replyHandoffFadeIn,
		"handoff phase %d is not an active handoff phase", phase)
	require.NotEmpty(t, fourthReplyRetiringGroups(m), "the oldest visible reply must retire (retiring batch non-empty)")
	require.NotEmpty(t, m.replyHandoff.Rows(), "the handoff must own rows")
	for _, r := range m.replyHandoff.Rows() {
		require.Truef(t, r.SourceIndex >= 0 && r.SourceIndex < len(m.views), "handoff row source %d is malformed", r.SourceIndex)
	}
	peakRows = len(m.replyHandoff.Rows())
	peakSources = vprlDistinctSources(m.replyHandoff.Rows())
	for range 500 {
		cmd := m.ar.Continue()
		if cmd == nil {
			break
		}
		tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
		require.True(t, ok, "tick must be accepted")
		_, _ = m.Update(tick)
		if !m.ReplyHandoffActive() {
			return peakRows, peakSources
		}
	}
	require.FailNow(t, "reply handoff did not retire through accepted ticks")
	return peakRows, peakSources
}

// vprlFinishStream ends the streamed reply, so the tail stops being active before the
// policy check. An offscreen completed tail must then be gone from both caches.
func vprlFinishStream(m *model) {
	_ = m.FinalizeStream()
	_ = m.View()
}

// vprlLoadRuntime loads sess with the deterministic handoff runtime and compact Work on, as
// the handoff fixture requires (workEnabled). It mirrors vprlLoad apart from the runtime.
func vprlLoadRuntime(tb testing.TB, sess *session.Session) *model {
	tb.Helper()
	ar := animation.NewRuntimeWithScheduler(&replyHandoffScheduler{now: testHandoffTime()})
	m := NewScrollableView(ar, 120, 40, &service.SessionState{}).(*model)
	tb.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	_ = m.SetCompactWorkLog(true)
	m.SetVisibleReplies(messages.VisibleRepliesThree)
	BeginReplay(m, PrepareReplay(sess), nil)
	drainRetainedReplay(tb, m)
	finishHistoryReplay(tb, m)
	_ = m.View()
	return m
}

// TestViewportRelayoutRepeatedScrollHandoffCounts scrolls one epoch (no SetSize, no theme
// change) through top, middle and bottom for three cycles. Each cycle runs one real reply
// handoff at the bottom. After each position and after each handoff, the retention window
// is checked as an exact subset, and owner counts are logged after a double GC.
func TestViewportRelayoutRepeatedScrollHandoffCounts(t *testing.T) {
	fixtures := []struct {
		name  string
		build func(testing.TB, int) *session.Session
	}{{"mixed", vprlRealSession}, {"heavy", vprlHeavySession}}
	for _, fx := range fixtures {
		for _, n := range vprlSizes {
			t.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(t *testing.T) {
				before := retainedHeapAfterGC()
				m := vprlLoadRuntime(t, fx.build(t, n))
				for cycle := 1; cycle <= 3; cycle++ {
					for _, pos := range []string{"top", "middle", "bottom"} {
						var target int
						bottom := max(0, m.layoutTotal()-m.height)
						switch pos {
						case "top":
							target = 0
						case "middle":
							target = bottom / 2
						default:
							target = bottom
						}
						vprlScrollToRow(t, m, target)
						vprlAssertRetention(t, m, fmt.Sprintf("cycle %d %s", cycle, pos))
					}
					peakRows, peakSources := vprlHandoffOnce(t, m, cycle)
					vprlAssertRetention(t, m, fmt.Sprintf("cycle %d after handoff", cycle))
					vprlFinishStream(m)
					vprlAssertRetention(t, m, fmt.Sprintf("cycle %d after stream end", cycle))
					for _, pos := range []string{"top", "middle"} {
						target := 0
						if pos == "middle" {
							target = max(0, m.layoutTotal()-m.height) / 2
						}
						vprlScrollToRow(t, m, target)
						vprlAssertRetention(t, m, fmt.Sprintf("cycle %d inactive %s", cycle, pos))
					}
					s := m.layoutWorkStats()
					after := retainedHeapAfterGC()
					t.Logf("owner cycle=%d fixture=%s n=%d cachedIDs=%v uncachedIDs=%v cached=%d uncached=%d handoffRows=%d handoffRowsPeak=%d handoffSourcesPeak=%d bodiesCached=%d bodiesUncached=%d bodiesHandoff=%d rowsCached=%d rowsUncached=%d rowsHandoff=%d facadeRows=%d renderedLines_len=%d renderedLines_cap=%d heap_delta_bytes=%d",
						cycle, fx.name, n, vprlKeys(m.renderedItems), vprlKeys(m.uncached), m.renderedItems.Len(), len(m.uncached), len(m.replyHandoff.Rows()), peakRows, peakSources,
						s.bodiesCached, s.bodiesUncached, s.bodiesHandoff, s.rowsCached, s.rowsUncached, s.rowsHandoff,
						s.facadeRows, len(m.renderedLines), cap(m.renderedLines), int64(after.HeapAlloc)-int64(before.HeapAlloc))
					runtime.KeepAlive(m)
				}
				m.StopAnimations()
			})
		}
	}
}
