package notification

import (
	"image"
	"slices"
	"time"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

const avoidanceDuration = 350 * time.Millisecond

// SetRuntime binds movement to the program's shared animation clock.
func (n *Manager) SetRuntime(ar *animation.Runtime) {
	if n.runtime == ar {
		return
	}
	n.motion.Cancel()
	n.motion.SetRuntime(ar)
	n.runtime = ar
	n.from, n.target = n.lift, n.lift
	n.syncAvoidance()
}

// SetAvoidance supplies actual occupied input cells in screen coordinates.
// Inactive or empty input restores the ordinary bottom-right placement.
func (n *Manager) SetAvoidance(cells []image.Rectangle, active bool) {
	n.occupied = slices.Clone(cells)
	n.avoidanceActive = active
	n.syncAvoidance()
}

// Cleanup releases movement work when the host stops displaying notifications.
func (n *Manager) Cleanup() {
	n.motion.Cancel()
	n.lift, n.from, n.target = 0, 0, 0
	n.occupied = nil
	n.avoidanceActive = false
}

func (n *Manager) syncAvoidance() {
	if !n.Open() {
		n.motion.Cancel()
		n.lift, n.from, n.target = 0, 0, 0
		return
	}
	target := n.avoidanceTarget()
	if target == n.target {
		return
	}
	n.from, n.target = n.lift, target
	if n.runtime == nil || n.from == target {
		n.motion.Cancel()
		n.lift = target
		return
	}
	n.motion.Start(avoidanceDuration, animation.EaseOutCubic)
}

func (n *Manager) avoidanceTarget() int {
	if !n.avoidanceActive || len(n.occupied) == 0 {
		return 0
	}
	bounds := n.restingBounds()
	if len(bounds) == 0 {
		return 0
	}

	// Always solve from the resting stack, never its moving hitboxes: otherwise
	// clearing the text during ascent would repeatedly reverse the target.
	lift, limit := 0, bounds[0].row
	screen := image.Rect(0, 0, n.width, n.height)
	for lift < limit {
		next := lift
		for _, b := range bounds {
			rect := image.Rect(b.col, b.row-lift, b.col+b.width, b.row+b.height-lift)
			for _, occupied := range n.occupied {
				occupied = occupied.Intersect(screen)
				if rect.Overlaps(occupied) {
					next = max(next, b.row+b.height-occupied.Min.Y)
				}
			}
		}
		if next == lift {
			break
		}
		lift = min(next, limit)
	}
	return lift
}
