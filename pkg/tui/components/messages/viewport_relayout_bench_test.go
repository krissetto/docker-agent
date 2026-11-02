package messages

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Opt-in benchmarks: run with -run '^$' -bench 'ViewportRelayout' -benchtime=2x.
// Every benchmark runs on both compositions (mixed and heavy) at 100, 1000 and
// 6000 session items. Each reports renderedMessages/op from the existing
// counter, measured only over the operations named in its comment.

// BenchmarkViewportRelayoutResize times SetSize and the following View, for
// alternating widths 120 and 90.
func BenchmarkViewportRelayoutResize(b *testing.B) {
	for _, fx := range vprlFixtures() {
		for _, n := range vprlSizes {
			b.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(b *testing.B) {
				m := vprlLoad(b, 120, fx.build(b, n))
				b.Cleanup(m.StopAnimations)
				start := vprlRenders(m)
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					if i%2 == 0 {
						m.SetSize(90, 40)
					} else {
						m.SetSize(120, 40)
					}
					_ = m.View()
				}
				b.StopTimer()
				b.ReportMetric(float64(vprlRenders(m)-start)/float64(b.N), "renderedMessages/op")
			})
		}
	}
}

// BenchmarkViewportRelayoutTheme times only the first View after a theme
// change. ApplyTheme runs untimed; the two variants alternate so every
// iteration is a real restyle.
func BenchmarkViewportRelayoutTheme(b *testing.B) {
	original := styles.CurrentTheme()
	b.Cleanup(func() { styles.ApplyTheme(original) })
	variants := [2]*styles.Theme{vprlTheme(original), original}
	for _, fx := range vprlFixtures() {
		for _, n := range vprlSizes {
			b.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(b *testing.B) {
				m := vprlLoad(b, 120, fx.build(b, n))
				b.Cleanup(m.StopAnimations)
				var rendered uint64
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					b.StopTimer()
					styles.ApplyTheme(variants[i%2])
					before := vprlRenders(m)
					b.StartTimer()
					_ = m.View()
					b.StopTimer()
					rendered += vprlRenders(m) - before
					b.StartTimer()
				}
				b.StopTimer()
				b.ReportMetric(float64(rendered)/float64(b.N), "renderedMessages/op")
			})
		}
	}
}

// BenchmarkViewportRelayoutScrollFirstStep is the isolated scroll-after-resize
// measurement. The resize and its first View are untimed, and the reader is at
// the top. Only the first wheel step and the View after it are timed.
func BenchmarkViewportRelayoutScrollFirstStep(b *testing.B) {
	for _, fx := range vprlFixtures() {
		for _, n := range vprlSizes {
			b.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(b *testing.B) {
				m := vprlLoad(b, 120, fx.build(b, n))
				b.Cleanup(m.StopAnimations)
				var rendered uint64
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					b.StopTimer()
					if i%2 == 0 {
						m.SetSize(90, 40)
					} else {
						m.SetSize(120, 40)
					}
					_ = m.View()
					m.scrollToTop()
					_ = m.View()
					before := vprlRenders(m)
					b.StartTimer()
					_, _ = m.Update(messages.WheelCoalescedMsg{Delta: 1, X: 5, Y: 5})
					_ = m.View()
					b.StopTimer()
					rendered += vprlRenders(m) - before
				}
				b.StopTimer()
				b.ReportMetric(float64(rendered)/float64(b.N), "renderedMessages/op")
			})
		}
	}
}

// BenchmarkViewportRelayoutResizeThenScroll is the end-to-end gesture: resize,
// its View, one wheel step and the View after it are all timed.
func BenchmarkViewportRelayoutResizeThenScroll(b *testing.B) {
	for _, fx := range vprlFixtures() {
		for _, n := range vprlSizes {
			b.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(b *testing.B) {
				m := vprlLoad(b, 120, fx.build(b, n))
				b.Cleanup(m.StopAnimations)
				m.scrollToTop()
				_ = m.View()
				start := vprlRenders(m)
				b.ReportAllocs()
				b.ResetTimer()
				for i := range b.N {
					if i%2 == 0 {
						m.SetSize(90, 40)
					} else {
						m.SetSize(120, 40)
					}
					_ = m.View()
					_, _ = m.Update(messages.WheelCoalescedMsg{Delta: 1, X: 5, Y: 5})
					_ = m.View()
				}
				b.StopTimer()
				b.ReportMetric(float64(vprlRenders(m)-start)/float64(b.N), "renderedMessages/op")
			})
		}
	}
}

// BenchmarkViewportRelayoutRetained reports heap retained after GC by a loaded
// and resized transcript: total bytes per operation and bytes per session item.
func BenchmarkViewportRelayoutRetained(b *testing.B) {
	for _, fx := range vprlFixtures() {
		for _, n := range vprlSizes {
			b.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(b *testing.B) {
				sess := fx.build(b, n)
				b.ReportAllocs()
				var retained int64
				for range b.N {
					before := retainedHeapAfterGC()
					m := vprlLoad(b, 120, sess)
					m.SetSize(90, 40)
					_ = m.View()
					after := retainedHeapAfterGC()
					retained += int64(after.HeapAlloc) - int64(before.HeapAlloc)
					runtime.KeepAlive(m)
					m.StopAnimations()
				}
				b.ReportMetric(float64(retained)/float64(b.N), "retained-bytes/op")
				b.ReportMetric(float64(retained)/float64(b.N)/float64(n), "retained-bytes/item")
				b.ReportMetric(float64(vprlContentBytes(sess)), "content-bytes/fixture")
			})
		}
	}
}
