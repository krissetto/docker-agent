package editor

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

type bannerHoverValue struct{ value, target float64 }

func (b *contextBar) renderPill(item bannerItem) string {
	return renderHoveredAttachmentPill(item, b.hoverValues[item.placeholder].value)
}

func (b *contextBar) hover(x, y int) tea.Cmd {
	if !b.hoverBound {
		return nil
	}
	active := map[string]bool{}
	if b.ToggleAt(x, y) {
		active["count"] = true
	}
	if item, ok := b.HitTestPosition(x, y); ok {
		active[item.placeholder] = true
	}
	if b.hoverValues == nil {
		b.hoverValues = make(map[string]bannerHoverValue)
	}
	for key := range active {
		if _, ok := b.hoverValues[key]; !ok {
			b.hoverValues[key] = bannerHoverValue{}
		}
	}
	running := false
	for key, state := range b.hoverValues {
		state.target = 0
		if active[key] {
			state.target = 1
		}
		b.hoverValues[key] = state
		running = running || state.value != state.target
	}
	if running {
		return b.hoverAnimation.Start()
	}
	b.hoverAnimation.Stop()
	return nil
}

func (b *contextBar) tickHover(tick animation.TickMsg) {
	if !b.hoverAnimation.IsActive() {
		return
	}
	before, after := tick.ElapsedBounds()
	changed, running := false, false
	for key, state := range b.hoverValues {
		old := state.value
		state.value = hoverStep(state.value, state.target, after-before)
		changed = changed || old != state.value
		running = running || state.value != state.target
		if state.value == 0 && state.target == 0 {
			delete(b.hoverValues, key)
		} else {
			b.hoverValues[key] = state
		}
	}
	if !running {
		b.hoverAnimation.Stop()
	}
	if changed {
		b.reflow()
		tick.MarkDirty()
	}
}

func (b *contextBar) cancelHover() {
	b.hoverAnimation.Stop()
	if len(b.hoverValues) > 0 {
		b.hoverValues = nil
		b.reflow()
	}
}

func hoverStep(value, target float64, elapsed time.Duration) float64 {
	step := float64(elapsed) / float64(animation.ShortDuration)
	if target > value {
		return min(target, value+step)
	}
	return max(target, value-step)
}
