package editor

import (
	"github.com/docker/docker-agent/pkg/tui/animation"
)

type bannerHoverValue struct{ value, target float64 }

func (b *contextBar) renderPill(item bannerItem) string {
	return renderHoveredAttachmentPill(item, b.hoverValues[item.placeholder].value)
}

func (b *contextBar) hover(x, y int) {
	if !b.hoverBound {
		return
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
		b.hoverAnimation.Start()
	} else {
		b.hoverAnimation.Stop()
	}
}

func (b *contextBar) tickHover(tick animation.TickMsg) {
	if !b.hoverAnimation.IsActive() {
		return
	}
	before, after := tick.ElapsedBounds()
	changed, running := false, false
	for key, state := range b.hoverValues {
		old := state.value
		state.value = animation.HoverStep(state.value, state.target, after-before)
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
	b.hoverValues = nil
}
