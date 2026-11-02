package messages

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

// TestViewportRelayoutRetainedMemoryReport logs the heap retained after GC by a
// loaded transcript, resized once, for each composition and size. The session
// is built before the baseline, so its own bytes are excluded. Gated: set
// VPRL_MEMORY=1 and run alone, one composition per process, to keep deltas clean.
func TestViewportRelayoutRetainedMemoryReport(t *testing.T) {
	if os.Getenv("VPRL_MEMORY") == "" {
		t.Skip("set VPRL_MEMORY=1 to run the retained-memory report")
	}
	for _, fx := range vprlFixtures() {
		for _, n := range vprlSizes {
			t.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(t *testing.T) {
				sess := fx.build(t, n)
				content := vprlContentBytes(sess)
				before := retainedHeapAfterGC()
				m := vprlLoad(t, 120, sess)
				m.SetSize(90, 40)
				_ = m.View()
				after := retainedHeapAfterGC()
				retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
				t.Logf("memory fixture=%s messages=%d content_bytes=%d retained_bytes=%d retained_bytes_per_item=%.0f retained_per_content_byte=%.2f",
					fx.name, n, content, retained, float64(retained)/float64(n), float64(retained)/float64(max(1, content)))
				runtime.KeepAlive(m)
				m.StopAnimations()
			})
		}
	}
}
