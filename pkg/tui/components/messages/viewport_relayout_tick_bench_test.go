package messages

import (
	"fmt"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// PROPOSAL ONLY (not in any worktree). Test-only measurement for one accepted
// animation tick and for work-status changes at 100, 1000 and 6000 items.
// Setup (fixture, load, status fixture) runs before ResetTimer. Tick acquisition
// (Continue and Accept) is outside the timed windows, so the timed region holds
// only Update(tick) and View, and the status calls and their Views.

// BenchmarkViewportRelayoutTick times the Update that fans one accepted tick
// out to the views, and the View after it, separately.
func BenchmarkViewportRelayoutTick(b *testing.B) {
	for _, n := range vprlSizes {
		b.Run(fmt.Sprintf("fixture=mixed/messages=%d", n), func(b *testing.B) {
			m := vprlLoad(b, 120, vprlRealSession(b, n))
			b.Cleanup(m.StopAnimations)
			// A pending tool call starts its spinner, the same activation the delegation
			// spinner tests use. Continue schedules a tick only while an animation is active.
			m.AddOrUpdateToolCall("root", tools.ToolCall{ID: "tick-spinner", Function: tools.FunctionCall{Name: "bench_spinner", Arguments: "{}"}}, tools.Tool{}, types.ToolStatusPending)
			_ = m.View()
			var updateTotal, viewTotal time.Duration
			start := vprlRenders(m)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				cmd := m.ar.Continue()
				if cmd == nil {
					b.Fatal("no animation tick: the pending tool spinner must keep an animation active")
				}
				tick, ok := m.ar.Accept(cmd().(animation.TickMsg))
				if !ok {
					b.Fatal("tick not accepted")
				}
				b.StartTimer()
				began := time.Now()
				_, _ = m.Update(tick)
				updateTotal += time.Since(began)
				began = time.Now()
				_ = m.View()
				viewTotal += time.Since(began)
			}
			b.StopTimer()
			ops := float64(b.N)
			b.ReportMetric(float64(updateTotal.Nanoseconds())/ops, "ns/update")
			b.ReportMetric(float64(viewTotal.Nanoseconds())/ops, "ns/view")
			b.ReportMetric(float64(vprlRenders(m)-start)/ops, "renderedMessages/op")
		})
	}
}

// BenchmarkViewportRelayoutWorkStatus isolates the timed SetWorkStatus(false)
// call (settle). Setup and the start state are untimed: each iteration first
// sets SetWorkStatus(true) and its View outside the timer. The timed region is
// only the settle call, reported as ns/status, and renderedMessages/status counts
// bodies that call rendered. The View after the settle is timed separately as
// ns/view-after and reports renderedMessages/view-after. Full re-render is not
// part of this measurement. Turns is half the item count.
func BenchmarkViewportRelayoutWorkStatus(b *testing.B) {
	for _, n := range vprlSizes {
		b.Run(fmt.Sprintf("work=true/messages=%d", n), func(b *testing.B) {
			m := buildWorkStatusSessionHistory(b, true, n/2)
			b.Cleanup(m.StopAnimations)
			_ = m.View()
			const settledFor = 2 * time.Second
			var statusTotal, viewTotal time.Duration
			var statusRenders, viewRenders uint64
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				m.SetWorkStatus(true, 0)
				_ = m.View()
				before := vprlRenders(m)
				b.StartTimer()
				began := time.Now()
				m.SetWorkStatus(false, settledFor)
				statusTotal += time.Since(began)
				b.StopTimer()
				statusRenders += vprlRenders(m) - before
				before = vprlRenders(m)
				began = time.Now()
				_ = m.View()
				viewTotal += time.Since(began)
				viewRenders += vprlRenders(m) - before
			}
			b.StopTimer()
			ops := float64(b.N)
			b.ReportMetric(float64(statusTotal.Nanoseconds())/ops, "ns/status")
			b.ReportMetric(float64(viewTotal.Nanoseconds())/ops, "ns/view-after")
			b.ReportMetric(float64(statusRenders)/ops, "renderedMessages/status")
			b.ReportMetric(float64(viewRenders)/ops, "renderedMessages/view-after")
		})
	}
}
