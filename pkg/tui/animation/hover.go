package animation

import (
	"math"
	"time"
)

// HoverStep shares the sidebar's reversible, constant-speed text highlight.
func HoverStep(value, target float64, elapsed time.Duration) float64 {
	step := float64(elapsed) / float64(150*time.Millisecond)
	if math.Abs(target-value) <= step+1e-12 {
		return target
	}
	if target > value {
		return min(target, value+step)
	}
	return max(target, value-step)
}
