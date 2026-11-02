package messages

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestViewportRelayoutResizeAllocationsScaleWithViewport is a structural guard
// that needs no production instrumentation. A resize plus View at a fixed
// viewport must allocate roughly the same amount whether the history holds 100
// or 6000 items. Work that walks all history or flattens every row grows with
// history and fails the ratio. Gated: VPRL_SCALING=1 (it builds 6000-item
// transcripts).
func TestViewportRelayoutResizeAllocationsScaleWithViewport(t *testing.T) {
	if os.Getenv("VPRL_SCALING") == "" {
		t.Skip("set VPRL_SCALING=1 to run the resize allocation scaling guard")
	}
	measure := func(n int) float64 {
		m := vprlLoad(t, 120, vprlRealSession(t, n))
		t.Cleanup(m.StopAnimations)
		m.scrollToTop()
		_ = m.View()
		step := 0
		return testing.AllocsPerRun(3, func() {
			if step%2 == 0 {
				m.SetSize(90, 40)
			} else {
				m.SetSize(120, 40)
			}
			step++
			_ = m.View()
		})
	}
	small := measure(100)
	large := measure(6000)
	t.Logf("resize allocs per op: messages=100 %.0f, messages=6000 %.0f, ratio %.2f", small, large, large/small)
	require.LessOrEqual(t, large, 3*small, "resize allocations must not grow with history: 100=%.0f 6000=%.0f", small, large)
}
