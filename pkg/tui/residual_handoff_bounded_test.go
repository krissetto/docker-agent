package tui

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// The initial reply handoff is a fixed, finite fade: 175ms, then 250ms and
// 175ms, 600ms total. At the real 16.67ms tick cadence that is 600/16.67 = 36
// ticks; 48 leaves margin for tick quantization. Reaching the hard cap means the
// handoff never finished and fails instead of settling.
const (
	residualHandoffExpectedTicks = 48
	residualHandoffTickCap       = 120
	residualHandoffRealStep      = 16666667 * time.Nanosecond
	residualIdleBaselineTicks    = 24
	// Measured 2026-10-28 after the messages single-pass tail fix, 37-tick handoff
	// with a stream chunk, 3 unprofiled runs: handoff/idle allocs per tick was
	// 1.74 (panes=1) and 1.63 (panes=3). 2.0 keeps about 15% margin above the worst.
	residualHandoffAllocRatioCeiling = 2.0
)

type replyHandoffOwner interface{ ReplyHandoffActive() bool }

// residualIdleAllocsPerTick measures steady owner ticks after the handoff, so the
// handoff's allocation budget is relative to this same fixture, not a machine ceiling.
func residualIdleAllocsPerTick(tb testing.TB, root *appModel, scheduler *paneReplayScheduler) float64 {
	tb.Helper()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	before := ms.Mallocs
	for range residualIdleBaselineTicks {
		residualTick(tb, root, scheduler)
		root.View()
	}
	runtime.ReadMemStats(&ms)
	return float64(ms.Mallocs-before) / residualIdleBaselineTicks
}

// Every tick of the handoff window is measured: the loop runs while the handoff
// is active, and nothing settles outside it.
func TestResidualInitialStreamHandoffBounded(t *testing.T) {
	for _, panes := range []int{1, 3} {
		t.Run(fmt.Sprintf("panes=%d", panes), func(t *testing.T) {
			root, scheduler := residualPaneFixture(t, "active", 3, panes, "long")
			owner, ok := root.chatPages["profile"].(replyHandoffOwner)
			require.True(t, ok, "chat page exposes the reply handoff state")
			scheduler.step = residualHandoffRealStep
			residualInput(root, "stream", -1)
			require.True(t, owner.ReplyHandoffActive(), "the initial reply chunk starts the handoff")

			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			allocsBefore := ms.Mallocs
			ticks, changed := 0, 0
			prev := root.View().Content
			for owner.ReplyHandoffActive() {
				require.Less(t, ticks, residualHandoffTickCap, "handoff exceeded its hard tick cap")
				require.NotEmpty(t, scheduler.pending, "an active handoff needs an owner tick lease")
				residualTick(t, root, scheduler)
				ticks++
				cur := root.View().Content
				if cur != prev {
					changed++
				}
				prev = cur
			}
			runtime.ReadMemStats(&ms)
			handoffAllocs := ms.Mallocs - allocsBefore

			require.LessOrEqual(t, ticks, residualHandoffExpectedTicks, "handoff completes within its fixed 600ms budget")
			require.Positive(t, changed, "the handoff changes visible frames")
			require.Contains(t, ansi.Strip(root.View().Content), "bounded stream", "final reply text is visible after the handoff")

			baseline := residualIdleAllocsPerTick(t, root, scheduler)
			perTick := float64(handoffAllocs) / float64(ticks)
			// ReadMemStats counts process-wide mallocs, and AllocsPerRun cannot repeat
			// this one-shot stateful window, so the assertion is finite and nonzero; the
			// upper bound is reported for approval against measured evidence.
			require.Positive(t, handoffAllocs, "the handoff window measures allocations")
			require.LessOrEqual(t, perTick/baseline, residualHandoffAllocRatioCeiling, "handoff allocations per tick stay within the measured idle-baseline ratio")
			t.Logf("handoff ticks=%d changed=%d allocs=%d allocs/tick=%.1f idle_baseline=%.1f", ticks, changed, handoffAllocs, perTick, baseline)
		})
	}
}
