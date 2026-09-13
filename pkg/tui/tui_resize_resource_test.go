package tui

import (
	goruntime "runtime"
	"strconv"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type resizeCacheReporter interface {
	ResizeCacheStats() (rebuilds, misses, renderedMessages uint64)
}

func TestRootAcceptedResizeWorkIsViewportBounded(t *testing.T) {
	var allocations []uint64
	for _, history := range []int{200, 600} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			root, _, _ := wallClockRoot(t, 120, 40)
			sess, _, _ := mixedHistorySession(history)
			root.application.Session().Messages = sess.Messages
			_ = root.chatPage.Init()
			root.handleWindowResize(120, 40)
			root.chatPage.ScrollToBottom()
			root.viewCacheValid = false
			_ = root.View()
			stats, ok := root.chatPage.(resizeCacheReporter)
			require.True(t, ok, "real chat must expose actual transcript render work")
			_, _ = root.Update(tea.MouseClickMsg{X: 40, Y: root.contentHeight, Button: tea.MouseLeft})
			resize := func(lines int) {
				y := root.height - lines - styles.EditorStyle.GetVerticalFrameSize() - root.tabBar.Height() - root.editor.BannerHeight() - 1
				motion := tea.MouseMotionMsg{X: 40, Y: y, Button: tea.MouseLeft}
				_, _ = root.Update(messages.PointerUpdateMsg{X: 40, Y: y, Motion: &motion})
			}
			// Warm both viewport extents before measuring repeated height-only work.
			resize(4)
			_ = root.View()
			resize(10)
			_ = root.View()
			rebuilds, misses, renders := stats.ResizeCacheStats()
			var before, after goruntime.MemStats
			goruntime.ReadMemStats(&before)
			started := time.Now()
			compositions := 0
			for i := range 20 {
				lines := 4
				if i%2 != 0 {
					lines = 10
				}
				resize(lines)
				require.Equal(t, lines-1, root.editorHeight, "accepted pointer must allocate in the same Update")
				require.False(t, root.editorHeightMotion.Running())
				if !root.viewCacheValid {
					compositions++
				}
				_ = root.View()
				resize(lines)
				require.True(t, root.viewCacheValid, "identical pointer rows are cache hits")
				_ = root.View()
			}
			elapsed := time.Since(started)
			goruntime.ReadMemStats(&after)
			nextRebuilds, nextMisses, nextRenders := stats.ResizeCacheStats()
			require.Equal(t, rebuilds, nextRebuilds, "height-only resize must not flatten the whole transcript")
			require.Equal(t, misses, nextMisses, "cached messages must not miss on height-only resize")
			require.Equal(t, renders, nextRenders, "height-only resize must not re-render message bodies")
			require.Equal(t, 20, compositions, "exactly one root composition per changed allocation")
			require.Zero(t, root.ar.ActiveCount(), "manual resizing owns no timer")
			allocations = append(allocations, after.TotalAlloc-before.TotalAlloc)
			t.Logf("history=%d accepted=40 changes=20 root_compositions=%d transcript_rebuilds=%d item_misses=%d message_renders=%d allocations=%d bytes=%d elapsed=%s bytes_per_change=%d elapsed_per_change=%s", history, compositions, nextRebuilds-rebuilds, nextMisses-misses, nextRenders-renders, after.Mallocs-before.Mallocs, after.TotalAlloc-before.TotalAlloc, elapsed, (after.TotalAlloc-before.TotalAlloc)/20, elapsed/20)
		})
	}
	require.Len(t, allocations, 2)
	require.LessOrEqual(t, allocations[1], allocations[0]*2, "tripling history must not triple accepted resize allocations")
}
