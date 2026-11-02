package messages

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLayoutRetainedMemoryStaysViewportBounded measures what a loaded, viewed
// transcript keeps on the heap. Only rows the viewport shows may hold text; the
// flattened mirror keeps its length, but its non-empty rows must not grow with
// history. Gated: VPRL_RETAINED=1 (it builds 6000-item transcripts).
func TestLayoutRetainedMemoryStaysViewportBounded(t *testing.T) {
	if os.Getenv("VPRL_RETAINED") != "1" {
		t.Skip("set VPRL_RETAINED=1 to run the retained layout measurement")
	}
	for _, n := range vprlSizes {
		t.Run(fmt.Sprintf("messages=%d", n), func(t *testing.T) {
			sess := vprlRealSession(t, n)
			before := retainedHeapAfterGC()
			m := vprlLoad(t, 120, sess)
			t.Cleanup(m.StopAnimations)
			after := retainedHeapAfterGC()

			nonEmpty := 0
			for _, row := range m.renderedLines {
				if row != "" {
					nonEmpty++
				}
			}
			retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			t.Logf("retained messages=%d heap_delta_bytes=%d heap_delta_per_item=%.0f renderedLines_len=%d renderedLines_cap=%d nonempty_rows=%d renderedItems=%d uncached=%d renders=%d visible_items=%d height=%d",
				n, retained, float64(retained)/float64(n), len(m.renderedLines), cap(m.renderedLines), nonEmpty,
				m.renderedItems.Len(), len(m.uncached), vprlRenders(m), vprlVisibleItems(m), m.height)

			// Materialized slots are bounded by the viewport item count; the rows they
			// hold are bounded by a constant multiple of the viewport height, which
			// admits a long item that straddles the viewport edge. Neither grows with N.
			materialized := m.renderedItems.Len() + len(m.uncached)
			require.LessOrEqual(t, materialized, vprlVisibleItems(m)+vprlAnchorSlack, "materialized slots must be viewport-bounded")
			require.LessOrEqual(t, nonEmpty, 4*m.height, "non-empty rows must be viewport-bounded, not N-proportional")
			runtime.KeepAlive(sess)
			runtime.KeepAlive(m)
		})
	}
}
