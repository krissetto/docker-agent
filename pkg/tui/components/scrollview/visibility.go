package scrollview

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/scrollbar"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// scrollbarHoverProximity keeps the hover target forgiving without stealing
	// content clicks farther away from the scrollbar gutter.
	scrollbarHoverProximity = 2
	scrollbarVisibilityHold = 2 * animation.MediumDuration
)

type scrollbarVisibility struct {
	runtime *animation.Runtime
	sub     animation.Subscription
	alpha   float64
	target  float64
	active  bool

	pointerX, pointerY int
	pointerKnown       bool
	hovered            bool
	dragging           bool
	holdUntil          time.Duration
	paint              scrollbarPaintCache
}

type scrollbarPaintCache struct {
	input  string
	alpha  float64
	theme  uint64
	output []string
	valid  bool
}

// WithAnimationRuntime opts a scrollview into automatic scrollbar visibility.
// Scrollviews without a runtime keep their static presentation for snapshots
// and reduced-motion embedders.
func WithAnimationRuntime(runtime *animation.Runtime) Option {
	return func(m *Model) { m.SetAnimationRuntime(runtime) }
}

// SetAnimationRuntime enables automatic scrollbar reveal/fade using the
// program-owned animation clock. Passing nil restores the static scrollbar.
func (m *Model) SetAnimationRuntime(runtime *animation.Runtime) {
	if m.visibility.runtime == runtime {
		return
	}
	wasBound := m.visibility.runtime != nil
	wasActive := m.visibility.active
	if m.visibility.sub.IsActive() {
		m.visibility.sub.Stop()
	}
	m.visibility.runtime = runtime
	if runtime == nil {
		m.visibility.sub = animation.Subscription{}
		m.visibility.active = false
		m.visibility.alpha = 1
		m.visibility.target = 1
		m.visibility.hovered = false
		m.visibility.dragging = false
		m.visibility.holdUntil = 0
		m.visualGeneration++
		return
	}
	m.visibility.sub = runtime.Subscribe()
	m.visibility.active = !wasBound || wasActive
	m.visibility.alpha = 0
	m.visibility.target = 0
	m.visibility.hovered = false
	m.visibility.dragging = false
	m.visibility.holdUntil = 0
	m.visualGeneration++
}

// SetScrollbarPointer updates the pointer's hover proximity without routing a
// mouse event or changing capture. Negative coordinates clear the pointer.
// It returns true when the visible presentation changed or began animating.
func (m *Model) SetScrollbarPointer(x, y int) bool {
	v := &m.visibility
	if v.runtime != nil && !v.active {
		return false
	}
	before := m.visualGeneration
	oldTarget := m.visibility.target
	if x < 0 || y < 0 {
		v.pointerKnown = false
	} else {
		v.pointerKnown = true
		v.pointerX, v.pointerY = x, y
	}
	m.updatePointerHover()
	m.reconcileVisibility()
	if oldTarget != m.visibility.target && before == m.visualGeneration {
		m.visualGeneration++
	}
	return before != m.visualGeneration
}

// UpdateAnimation advances automatic scrollbar visibility. Owners should call
// it only for accepted animation ticks and invalidate their view when true.
func (m *Model) UpdateAnimation(msg tea.Msg) bool {
	tick, ok := msg.(animation.TickMsg)
	if !ok || m.visibility.runtime == nil || !m.visibility.sub.IsActive() {
		return false
	}
	before, after := tick.ElapsedBounds()
	v := &m.visibility
	old := v.alpha
	oldTarget := v.target
	m.reconcileVisibility()
	v.alpha = animation.HoverStep(v.alpha, v.target, after-before)
	if old != v.alpha {
		m.visualGeneration++
		tick.MarkDirty()
	}
	if v.alpha == v.target && (v.hovered || v.runtime.Now() >= v.holdUntil) {
		v.sub.Stop()
	}
	return old != v.alpha || oldTarget != v.target
}

// SetScrollbarActive suspends or resumes scrollbar presentation independently
// from component data updates. Resuming waits for a fresh pointer position or a
// real offset change before revealing. It returns true when output changes.
func (m *Model) SetScrollbarActive(active bool) bool {
	v := &m.visibility
	if v.runtime == nil {
		return false
	}
	if v.active == active {
		return false
	}
	if !active {
		m.StopAnimation()
		return true
	}
	v.active = true
	v.pointerKnown = false
	v.hovered = false
	v.dragging = m.sb.IsDragging()
	v.holdUntil = 0
	v.target = 0
	if v.alpha != 0 {
		v.alpha = 0
	}
	m.visualGeneration++
	return true
}

// StopAnimation releases the visibility lease and clears pointer state. A
// hidden, runtime-bound scrollview starts fully hidden when shown again.
func (m *Model) StopAnimation() {
	v := &m.visibility
	active := v.sub.IsActive() || v.alpha != 0 || v.hovered || v.dragging
	v.active = false
	if v.sub.IsActive() {
		v.sub.Stop()
	}
	v.pointerKnown = false
	v.hovered = false
	v.dragging = false
	v.holdUntil = 0
	if m.sb.IsDragging() {
		_, _ = m.sb.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft})
	}
	if v.runtime != nil {
		v.alpha = 0
		v.target = 0
	} else {
		v.alpha = 1
	}
	if active {
		m.visualGeneration++
	}
}

func (m *Model) scrollbarOpacity() float64 {
	if m.visibility.runtime == nil {
		return 1
	}
	return m.visibility.alpha
}

func (m *Model) visibilityTarget() float64 {
	v := &m.visibility
	if v.runtime == nil {
		return 1
	}
	if !v.active {
		return 0
	}
	if v.hovered || v.dragging || v.runtime.Now() < v.holdUntil {
		return 1
	}
	return 0
}

func (m *Model) noteScrollActivity() {
	v := &m.visibility
	if v.runtime == nil || !v.active || !m.NeedsScrollbar() {
		return
	}
	v.holdUntil = v.runtime.Now() + scrollbarVisibilityHold
	m.reconcileVisibility()
}

func (m *Model) updatePointerHover() {
	v := &m.visibility
	hovered := false
	if v.runtime != nil && v.active && v.pointerKnown && m.NeedsScrollbar() && m.height > 0 {
		left := m.ScrollbarX() - scrollbarHoverProximity
		right := m.ScrollbarX() + scrollbar.Width - 1 + scrollbarHoverProximity
		hovered = v.pointerX >= left && v.pointerX <= right && v.pointerY >= m.yPos && v.pointerY < m.yPos+m.height
	}
	if hovered == v.hovered {
		return
	}
	v.hovered = hovered
	if hovered {
		v.holdUntil = v.runtime.Now() + scrollbarVisibilityHold
	}
}

func (m *Model) reconcileVisibility() {
	v := &m.visibility
	if v.runtime == nil {
		return
	}
	if !m.NeedsScrollbar() || !v.active {
		if v.sub.IsActive() {
			v.sub.Stop()
		}
		v.alpha, v.target, v.holdUntil = 0, 0, 0
		return
	}
	target := m.visibilityTarget()
	v.target = target
	needsTick := v.alpha != target || (v.holdUntil > 0 && v.runtime.Now() < v.holdUntil && !v.hovered && !v.dragging)
	if needsTick {
		v.sub.Start()
	} else {
		v.sub.Stop()
	}
}

func (m *Model) composeScrollbar(lines []string) []string {
	input := strings.Join(lines, "\n")
	alpha, theme := m.scrollbarOpacity(), styles.ThemeGeneration()
	cache := &m.visibility.paint
	if cache.valid && cache.input == input && cache.alpha == alpha && cache.theme == theme {
		return cache.output
	}
	painted := input
	if m.scrollbarDecorator != nil {
		painted = m.scrollbarDecorator(painted)
	}
	result := strings.Split(painted, "\n")
	if alpha <= 0 {
		for i := range result {
			result[i] = " "
		}
	} else if alpha < 1 {
		fc := styles.NewFadeContext()
		for i := range result {
			result[i] = styles.FadeLineCtx(result[i], alpha, &fc)
		}
	}
	cache.input, cache.alpha, cache.theme = input, alpha, theme
	cache.output, cache.valid = result, true
	return cache.output
}

// AnimationActive reports whether scrollbar reveal or fade is awaiting ticks.
// Owners must keep their own tick subscription alive while this is true.
func (m *Model) AnimationActive() bool { return m.visibility.sub.IsActive() }
