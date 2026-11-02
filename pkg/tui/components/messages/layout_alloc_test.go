package messages

import (
	"crypto/sha256"
	"fmt"
	"math/bits"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// allocEventBytes returns the bytes allocated by one resize event followed by a
// View, measured on the real layout path with the test's counted fixtures.
func allocEventBytes(t *testing.T, items int, event func(m *model)) uint64 {
	t.Helper()
	m := NewScrollableView(animation.NewRuntime(), 120, 40, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	for i := range items {
		view := &countedHistoryView{content: fmt.Sprintf("message %d", i)}
		m.messages = append(m.messages, types.User(view.content))
		m.views = append(m.views, view)
	}
	_ = m.View()
	m.userHasScrolled = true
	m.setScrollOffset(m.totalHeight / 2)
	_ = m.View()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	event(m)
	_ = m.View()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestLayoutAllocationsDoNotScaleWithHistory compares the bytes allocated by a
// dramatic width shrink (120 to 20) and grow back, at 1000 and 6000 items. The
// per-event allocation must stay about the same at both sizes: a shrink that
// allocates history-sized arrays would scale about 6x.
func TestLayoutAllocationsDoNotScaleWithHistory(t *testing.T) {
	events := map[string]func(m *model){
		"shrink-120-to-20": func(m *model) { m.SetSize(20, 40) },
		"grow-20-to-120":   func(m *model) { m.SetSize(120, 40) },
	}
	for name, event := range events {
		small := allocEventBytes(t, 1000, event)
		large := allocEventBytes(t, 6000, event)
		t.Logf("%s bytes/event 1000=%d 6000=%d ratio=%.2f", name, small, large, float64(large)/float64(max(small, 1)))
		if large > 2*small+64*1024 {
			t.Errorf("%s allocates with history: %d bytes at 6000 against %d at 1000", name, large, small)
		}
	}
}

// allocGuardFiles are the production files the adversarial guard must not see
// change while it runs: a source edit mid-run would make the numbers meaningless.
var allocGuardFiles = []string{"layout_slots.go", "messages.go"}

// allocGuardHashes returns the SHA-256 of each guarded source file.
func allocGuardHashes(t *testing.T) map[string][32]byte {
	t.Helper()
	out := make(map[string][32]byte, len(allocGuardFiles))
	for _, name := range allocGuardFiles {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		out[name] = sha256.Sum256(data)
	}
	return out
}

// allocCost is one event's cost: bytes allocated plus the layout counter deltas
// the viewport bounds apply to.
type allocCost struct {
	bytes                                                                          uint64
	renders, itemVisits, sizeCalls, projectionVisits, slotMetaVisits               uint64
	slotLoops, facadeEntries, indexNodes, rowsAllocated, rowsCleared, slotRebuilds uint64
}

// allocMeasure runs event and the View after it, and returns the bytes it
// allocated and the counter deltas. Snapshots are taken outside the memory
// window, so their own allocations are not charged to the event.
func allocMeasure(t *testing.T, m *model, event func(*model)) allocCost {
	t.Helper()
	s0 := m.layoutWorkStats()
	r0 := vprlRenders(m)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	event(m)
	_ = m.View()
	runtime.ReadMemStats(&after)
	s1 := m.layoutWorkStats()
	return allocCost{
		bytes:            after.TotalAlloc - before.TotalAlloc,
		renders:          vprlRenders(m) - r0,
		itemVisits:       s1.itemVisits - s0.itemVisits,
		sizeCalls:        s1.sizeCalls - s0.sizeCalls,
		projectionVisits: s1.projectionVisits - s0.projectionVisits,
		slotMetaVisits:   s1.slotMetaVisits - s0.slotMetaVisits,
		slotLoops:        s1.slotLoops - s0.slotLoops,
		facadeEntries:    s1.facadeEntries - s0.facadeEntries,
		indexNodes:       s1.indexNodes - s0.indexNodes,
		rowsAllocated:    s1.rowsAllocated - s0.rowsAllocated,
		rowsCleared:      s1.rowsCleared - s0.rowsCleared,
		slotRebuilds:     s1.slotRebuilds - s0.slotRebuilds,
	}
}

// allocLongBody is the one very long visible message: the heavy document eight
// times over, so it holds markdown, code fences and tables at every width.
// Its size: 68,680 source bytes and 8,968 words. Rendered rows: 3,288 at width
// 200 and 9,024 at width 20. Each event re-renders it exactly once (renders=1), so
// its allocation is the one per-event cost that scales with the item, not history.
//
// Attribution, sampled alloc_space over the three events at 6000 items (profile
// mem-6000.out, 167.4 MB of event allocation out of 28.7 GB total, where the rest is
// the initial load): lipgloss Style.Render in styledAssistantLines (per-row pad,
// wrap, border and align) 129.9 MB (77.6%); markdown IncrementalRenderer.RenderParts
// (parse, code-block highlight, tables) 36.9 MB (22.0%); other 0.6 MB (0.4%);
// BuildSourceMap 0; facade publish (publishFacade) 0. The shrink, grow and theme
// kinds share one stack shape, so this table covers all three together. Per-kind
// bytes come from MemStats (see the test log).
func allocLongBody(t *testing.T) string {
	t.Helper()
	return strings.Repeat(vprlHeavyDoc(t)+"\n\n", 8)
}

// allocModel loads n heavy answers plus one very long final answer through the
// production replay path, pinned to the bottom so the long answer is visible.
// It returns the model and the index of the long answer.
func allocModel(t *testing.T, n int) (*model, int) {
	t.Helper()
	sess := vprlHeavySession(t, n)
	sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: allocLongBody(t)}))
	m := vprlLoad(t, 200, sess)
	t.Cleanup(m.StopAnimations)
	_ = m.scrollToBottom()
	_ = m.View()
	return m, len(m.views) - 1
}

// allocBytesPerSourceByte caps one event's allocation per byte of the long answer's
// source. The measured figure is 720 (shrink to 20), 891 (grow to 200) and 886 (theme)
// bytes per source byte at 6000 items, so the cap leaves room for about 15% drift.
// It is a guard against a regression to history-scaled work, not an explanation of
// the cost: the cost is the single long answer re-rendered once per event.
const allocBytesPerSourceByte = 1024

// TestLayoutAllocAdversarialCapGuard drives 1000 and 6000 heavy answers through a
// width shrink to 20, a grow back to 200 and a theme change. Each event must cost
// viewport-bounded counter work, and its allocation must not scale with history.
// The long visible answer alone can be large at width 20: its rows and bytes are
// its own size, so those bounds use the measured size of that one item, never N.
func TestLayoutAllocAdversarialCapGuard(t *testing.T) {
	before := allocGuardHashes(t)
	t.Cleanup(func() {
		after := allocGuardHashes(t)
		for _, name := range allocGuardFiles {
			require.Equal(t, before[name], after[name], "%s changed while the guard ran", name)
		}
	})

	const height = 40
	// Visible slots: one row each at least, plus the edge slots, the anchored row
	// and the bottom window's partly visible item. Independent of N.
	const slotBound = height + 8
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	variant := vprlTheme(original)

	type measured struct {
		events map[string]allocCost
	}
	results := map[int]measured{}
	longSource := allocLongBody(t)
	for _, n := range []int{1000, 6000} {
		t.Run(fmt.Sprintf("heavy=%d", n), func(t *testing.T) {
			m, long := allocModel(t, n)
			require.Len(t, m.views, n+1)
			out := measured{events: map[string]allocCost{}}
			steps := []struct {
				name string
				run  func(*model)
			}{
				{"shrink-200-to-20", func(m *model) { m.SetSize(20, height) }},
				{"grow-20-to-200", func(m *model) { m.SetSize(200, height) }},
				{"theme", func(m *model) {
					styles.ApplyTheme(variant)
					_, _ = m.Update(messages.ThemeChangedMsg{})
				}},
			}
			for _, step := range steps {
				cost := allocMeasure(t, m, step.run)
				out.events[step.name] = cost
				longRows := uint64(m.geom.Height(long))
				t.Logf("n=%d %s bytes=%d renders=%d itemVisits=%d sizeCalls=%d projectionVisits=%d slotMetaVisits=%d slotLoops=%d facadeEntries=%d indexNodes=%d rowsAllocated=%d rowsCleared=%d slotRebuilds=%d longRows=%d totalRows=%d",
					n, step.name, cost.bytes, cost.renders, cost.itemVisits, cost.sizeCalls, cost.projectionVisits, cost.slotMetaVisits,
					cost.slotLoops, cost.facadeEntries, cost.indexNodes, cost.rowsAllocated, cost.rowsCleared, cost.slotRebuilds,
					m.geom.Height(long), m.layoutTotal())
				require.Zero(t, cost.slotRebuilds, "%s needs no full slot index rebuild", step.name)
				require.LessOrEqual(t, cost.renders, uint64(slotBound), "%s renders only viewport-bounded bodies", step.name)
				require.LessOrEqual(t, cost.itemVisits, uint64(slotBound), "%s measures only viewport-bounded slots", step.name)
				require.LessOrEqual(t, cost.sizeCalls, uint64(slotBound), "%s sizes only viewport-bounded views", step.name)
				require.LessOrEqual(t, cost.projectionVisits, uint64(slotBound), "%s visits no global projection", step.name)
				require.LessOrEqual(t, cost.slotMetaVisits, uint64(slotBound), "%s updates only viewport-bounded slot metadata", step.name)
				require.LessOrEqual(t, cost.slotLoops, uint64(slotBound), "%s runs no N-proportional slot loop", step.name)
				require.LessOrEqual(t, cost.indexNodes, uint64(slotBound*4*bits.Len(uint(n+1))), "%s index work is viewport slots times O(log N)", step.name)
				// Rows: the viewport rows plus the long answer, which is one item and can
				// hold its whole height of rows at this width. Its own size is the only
				// history-independent term here.
				require.LessOrEqual(t, cost.rowsAllocated, 2*height+2*longRows, "%s allocates no history-sized row capacity", step.name)
				require.LessOrEqual(t, cost.rowsCleared, 2*height+2*longRows, "%s clears no history-sized rows", step.name)
				require.LessOrEqual(t, cost.facadeEntries, uint64(slotBound), "%s places only viewport-bounded facade entries", step.name)
				// Absolute bytes: the event renders the long answer once, so its own source
				// size bounds it. Measured about 890 bytes per source byte at width 20 and 200.
				require.LessOrEqual(t, cost.bytes, allocBytesPerSourceByte*uint64(len(longSource)), "%s allocates beyond the long answer's own size", step.name)
			}
			results[n] = out
		})
	}
	small, large := results[1000], results[6000]
	for _, name := range []string{"shrink-200-to-20", "grow-20-to-200", "theme"} {
		s, l := small.events[name], large.events[name]
		t.Logf("%s bytes/event 1000=%d 6000=%d ratio=%.2f", name, s.bytes, l.bytes, float64(l.bytes)/float64(max(s.bytes, 1)))
		require.LessOrEqual(t, float64(l.bytes), 1.5*float64(max(s.bytes, 1)), "%s allocates with history: %d bytes at 6000 against %d at 1000", name, l.bytes, s.bytes)
	}
}
