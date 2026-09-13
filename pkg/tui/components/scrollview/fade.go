package scrollview

import (
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// FadeHeightPercent is the fraction of viewport height used for the fade
	// gradient on each edge.
	FadeHeightPercent = 0.15

	// FadeMinOpacity is the opacity floor for the outermost (most faded)
	// line. The gradient interpolates from FadeMinOpacity to 1.0, keeping
	// even the edge lines slightly visible for a gentler transition.
	FadeMinOpacity = 0.50

	// MinFadeLines is the minimum number of lines in the fade gradient,
	// ensuring visibility even on very small viewports.
	MinFadeLines = 1

	// MaxFadeLines is the maximum number of lines in the fade gradient,
	// preventing the effect from consuming too much of the viewport.
	MaxFadeLines = 20
)

// fadeLinesForHeight returns the number of fade gradient lines based on
// the viewport height.
func fadeLinesForHeight(height int) int {
	n := int(float64(height) * FadeHeightPercent)
	return max(MinFadeLines, min(n, MaxFadeLines))
}

type fadeCacheEntry struct {
	line   string
	alpha  float64
	output string
}

type fadeCache struct {
	context styles.FadeContext
	valid   bool
	top     []fadeCacheEntry
	bottom  []fadeCacheEntry
}

// applyFade overlays a foreground-color gradient on edge lines.
// Top lines fade when scrollOffset > 0; bottom lines fade when content
// extends below the visible area.
func (m *Model) applyFade(lines []string) {
	fadeLines := fadeLinesForHeight(m.height)
	n := min(fadeLines, len(lines)/2)
	if n <= 0 {
		return
	}

	hasAbove := m.scrollOffset > 0
	hasBelow := m.scrollOffset+m.height < m.totalHeight
	if !hasAbove && !hasBelow {
		return
	}

	fc := styles.NewFadeContext()
	if !m.fadeCache.valid || m.fadeCache.context != fc {
		m.fadeCache = fadeCache{context: fc, valid: true}
	}
	m.fadeCache.top = resizeFadeCache(m.fadeCache.top, n)
	m.fadeCache.bottom = resizeFadeCache(m.fadeCache.bottom, n)

	if hasAbove {
		for i := range n {
			lines[i] = cachedFadeLine(lines[i], fadeAlpha(i, n), &fc, &m.fadeCache.top[i])
		}
	}
	if hasBelow {
		for i := range n {
			idx := len(lines) - 1 - i
			lines[idx] = cachedFadeLine(lines[idx], fadeAlpha(i, n), &fc, &m.fadeCache.bottom[i])
		}
	}
}

func resizeFadeCache(entries []fadeCacheEntry, n int) []fadeCacheEntry {
	if cap(entries) < n {
		return make([]fadeCacheEntry, n)
	}
	return entries[:n]
}

func cachedFadeLine(line string, alpha float64, fc *styles.FadeContext, entry *fadeCacheEntry) string {
	if entry.line != line || entry.alpha != alpha {
		*entry = fadeCacheEntry{line: line, alpha: alpha, output: styles.FadeLineCtx(line, alpha, fc)}
	}
	return entry.output
}

// fadeAlpha returns the opacity for fade line i of n (0 = edge, n-1 = innermost).
// The result is mapped to [FadeMinOpacity, 1.0] so even the outermost line
// retains some visibility.
func fadeAlpha(i, n int) float64 {
	t := float64(i+1) / float64(n+1)
	return FadeMinOpacity + (1-FadeMinOpacity)*t
}
