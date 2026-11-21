package tabbar

import (
	"image"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// dragMotion holds one position per original visible tab. The floating tab
// follows the mouse until release; then the same transition moves it and any
// bystanders that haven't reached their slots yet.
type dragMotion struct {
	transition animation.Transition
	positions  []tabPosition
	floatX     int
	canvas     *lipgloss.Canvas // reused by the one-line renderer

	reordering bool
	settling   bool
	applied    bool // SetTabs has acknowledged this drop's expected order
	from, to   int  // indices in the original and final order, respectively
}

type tabPosition struct{ x, from, to int }

func (d dragState) visualDrop() int {
	if d.isNoOp() {
		return noTab
	}
	return d.dropIdx
}

func (m *dragMotion) lift(bounds []tabBound) {
	m.positions = m.positions[:0]
	for _, b := range bounds {
		m.positions = append(m.positions, tabPosition{x: b.start, from: b.start, to: b.start})
	}
}

func (t *TabBar) floatingX(mouseX int) int {
	source := t.dragBounds[t.drag.dragIdx]
	return t.clampFloatingX(mouseX-t.drag.grab, source.end-source.start)
}

func (t *TabBar) reflow(drop int) tea.Cmd {
	m := &t.motion
	from := t.drag.dragIdx
	source := t.dragBounds[from]
	width := source.end - source.start
	moving := false
	for idx, b := range t.dragBounds {
		if idx == from {
			continue
		}
		target := siblingTarget(b.start, width, idx, from, drop)
		p := &m.positions[idx]
		p.from, p.to = p.x, target
		moving = moving || p.from != p.to
	}
	if !moving {
		m.transition.Cancel()
		return nil
	}
	return m.transition.Start(dragReflowDuration, animation.EaseOutQuint)
}

func (t *TabBar) settle(x, from, insertion, finalTo int) tea.Cmd {
	m := &t.motion
	m.applied = false
	m.from, m.to = from, finalTo
	target := t.settleTarget(from, insertion)
	m.floatX = t.floatingX(x)
	moving := m.floatX != target
	for i := range m.positions {
		p := &m.positions[i]
		p.from = p.x
		moving = moving || p.from != p.to
	}
	m.positions[from] = tabPosition{x: m.floatX, from: m.floatX, to: target}
	m.settling = moving
	if !moving {
		m.transition.Cancel()
		return nil
	}
	return m.transition.Start(tabSettleDuration, animation.EaseOutQuint)
}

func (t *TabBar) settleTarget(from, insertion int) int {
	source := t.dragBounds[from]
	width := source.end - source.start
	target := source.start
	if insertion > from {
		target = t.dragBounds[insertion-1].end - width
	} else if insertion < from {
		target = t.dragBounds[insertion].start
	}
	return t.clampFloatingX(target, width)
}

func (t *TabBar) tickMotion(msg animation.TickMsg) {
	m := &t.motion
	if !m.transition.Running() {
		return
	}
	m.transition.Tick()
	changed := false
	for i := range m.positions {
		if t.drag.active && i == t.drag.dragIdx {
			continue
		}
		p := &m.positions[i]
		x := m.transition.Lerp(p.from, p.to)
		changed = changed || x != p.x
		p.x = x
	}
	if (m.settling || m.reordering) && !m.transition.Running() {
		m.settling, m.reordering = false, false
		changed = true // The floating tab returns to its normal styling.
	}
	if changed {
		t.visualGeneration++
		msg.MarkDirty()
	}
}

func (t *TabBar) cancelDrag() {
	if t.HasFloatingOverlay() || t.motion.reordering {
		t.visualGeneration++
	}
	t.drag = dragState{dropIdx: noTab}
	t.motion.transition.Cancel()
	t.motion.settling, t.motion.reordering = false, false
}

func sameOrder(before, after []messages.TabInfo) bool {
	return slices.EqualFunc(before, after, func(a, b messages.TabInfo) bool {
		return a.SessionID == b.SessionID
	})
}

func (m *dragMotion) matchesReorder(before, after []messages.TabInfo) bool {
	if m.applied || len(before) != len(after) {
		return false
	}
	for i, tab := range before {
		if tab.SessionID != after[m.currentIndex(i)].SessionID {
			return false
		}
	}
	return true
}

// currentIndex maps indices in the original drag layout to the reordered tabs.
func (m *dragMotion) currentIndex(i int) int {
	switch {
	case i == m.from:
		return m.to
	case m.from < i && i <= m.to:
		return i - 1
	case m.to <= i && i < m.from:
		return i + 1
	default:
		return i
	}
}

func (t *TabBar) addTabZones(idx int, tab Tab, left, right int) {
	end := min(left+tab.Width(), right)
	if end <= left {
		return
	}
	mainEnd := min(left+tab.MainZoneEnd(), end)
	t.zones = append(t.zones,
		clickZone{startX: left, endX: mainEnd, tabIdx: idx},
		clickZone{startX: mainEnd, endX: end, tabIdx: idx, isClose: true},
	)
}

// FloatingTab describes the topmost tab in tab-bar-local coordinates.
type FloatingTab struct {
	Content string
	X       int
	tab     Tab
	index   int
}

// FloatingOverlay keeps the source above strip chrome at the original grab point.
func (t *TabBar) FloatingOverlay() *FloatingTab {
	if !t.HasFloatingOverlay() {
		return nil
	}
	idx, x := t.drag.dragIdx, t.motion.floatX
	if t.motion.settling {
		idx = t.motion.from
		x = t.motion.positions[idx].x
		if t.motion.applied {
			idx = t.motion.to
		}
	}
	tab := renderTab(t.tabs[idx], t.maxTitleLen, dragRoleSource, t.ar.Now())
	return &FloatingTab{Content: ansi.Truncate(tab.View(), t.availableWidth(), ""), X: t.clampFloatingX(x, tab.Width()), tab: tab, index: idx}
}

// renderMotion draws only the siblings; the compositor owns the lifted tab.
func (t *TabBar) renderMotion(tabs []Tab) string {
	m := &t.motion
	from := noTab
	if t.drag.active {
		from = t.drag.dragIdx
	} else if m.settling {
		from = m.from
	}
	left, right := t.dragBounds[t.dragOffset].start, t.dragRight
	if m.canvas == nil {
		m.canvas = lipgloss.NewCanvas(right-left, tabBarHeight)
	} else if m.canvas.Width() != right-left {
		m.canvas.Resize(right-left, tabBarHeight)
	}
	m.canvas.Clear()
	for originalIdx, p := range m.positions {
		if originalIdx == from {
			continue
		}
		idx := originalIdx
		if m.settling && m.applied {
			idx = m.currentIndex(originalIdx)
		}
		tab := tabs[idx]
		x, end := max(left, p.x), min(p.x+tab.Width(), right)
		if x >= end {
			continue
		}
		content := ansi.Cut(tab.View(), x-p.x, end-p.x)
		uv.NewStyledString(content).Draw(m.canvas, image.Rect(x-left, 0, end-left, tabBarHeight))
		mainEnd := min(max(x, p.x+tab.MainZoneEnd()), end)
		t.zones = append(t.zones,
			clickZone{startX: x, endX: mainEnd, tabIdx: idx},
			clickZone{startX: mainEnd, endX: end, tabIdx: idx, isClose: true},
		)
	}
	line := m.canvas.Render()
	return line + strings.Repeat(" ", max(0, right-left-lipgloss.Width(line)))
}

func (t *TabBar) availableWidth() int {
	if t.width > 0 {
		return t.width
	}
	return fallbackWidth
}

func (t *TabBar) clampFloatingX(x, width int) int {
	return min(max(0, x), max(0, t.availableWidth()-width))
}

func (t *TabBar) cacheTabBounds(tabs []Tab, offset, left int) {
	t.dragOffset = offset
	t.dragBounds = slices.Grow(t.dragBounds[:0], len(tabs))[:len(tabs)]
	for i := offset - 1; i >= 0; i-- {
		left -= tabs[i].Width()
	}
	for i, tab := range tabs {
		x := left + tab.Width()
		t.dragBounds[i] = tabBound{start: left, end: x}
		left = x
	}
}

func (t *TabBar) cacheVisibleBounds(last, right int) {
	t.dragLast, t.dragRight = last, right
}

func (t *TabBar) motionTabs(infos []messages.TabInfo) []Tab {
	tabs := make([]Tab, len(infos))
	for i, info := range infos {
		tabs[i] = renderTab(info, t.maxTitleLen, dragRoleNone, t.ar.Now())
	}
	return tabs
}

// updateMotionGeometry rebuilds the underlying strip and keeps a live drag's
// grab point or a settling overlay's current position across width changes.
func (t *TabBar) updateMotionGeometry() {
	if !t.HasFloatingOverlay() && !t.motion.reordering {
		return
	}
	tabs := t.motionTabs(t.tabs)
	m := &t.motion
	if m.settling && m.applied {
		original := make([]Tab, len(tabs))
		for i := range original {
			original[i] = tabs[m.currentIndex(i)]
		}
		tabs = original
	}
	width := plusButtonWidth
	for _, tab := range tabs {
		width += tab.Width()
	}
	offset := t.dragOffset
	if width <= t.availableWidth() {
		offset = 0
	}
	left := 0
	if offset > 0 {
		left = scrollArrowWidth
	}
	geometryMatches := offset == t.dragOffset && len(t.dragBounds) == len(tabs)
	if geometryMatches {
		for i, tab := range tabs {
			if t.dragBounds[i].end-t.dragBounds[i].start != tab.Width() {
				geometryMatches = false
				break
			}
		}
	}
	t.cacheTabBounds(tabs, offset, left)
	last, right := offset, t.dragBounds[offset].end
	for i := offset + 1; i < len(tabs); i++ {
		reserve := plusButtonWidth
		if i < len(tabs)-1 {
			reserve += scrollArrowWidth
		}
		if t.dragBounds[i].end+reserve > t.availableWidth() {
			break
		}
		last, right = i, t.dragBounds[i].end
	}
	t.cacheVisibleBounds(last, right)
	switch {
	case t.drag.active:
		t.motion.floatX = t.floatingX(t.drag.cursorX)
		if !geometryMatches {
			m.resetSiblings(t.dragBounds, t.drag.dragIdx, t.drag.visualDrop())
		}
	case m.settling:
		insertion := m.to
		if m.to > m.from {
			insertion++
		}
		target := t.settleTarget(m.from, insertion)
		if !geometryMatches {
			drop := insertion
			if m.from == m.to {
				drop = noTab
			}
			m.resetSiblings(t.dragBounds, m.from, drop)
		}
		p := &m.positions[m.from]
		if p.to != target {
			p.from, p.to = p.x, target
			_ = m.transition.Start(tabSettleDuration, animation.EaseOutQuint)
		}
	case !geometryMatches:
		t.cancelDrag()
	}
}

func (m *dragMotion) resetSiblings(bounds []tabBound, source, drop int) {
	width := bounds[source].end - bounds[source].start
	for i, b := range bounds {
		if i == source {
			continue
		}
		x := siblingTarget(b.start, width, i, source, drop)
		m.positions[i] = tabPosition{x: x, from: x, to: x}
	}
}

func siblingTarget(start, width, index, source, drop int) int {
	switch {
	case drop == noTab:
		return start
	case source < index && index < drop:
		return start - width
	case drop <= index && index < source:
		return start + width
	default:
		return start
	}
}

// animateReorder follows stable IDs for a pure reorder not initiated by a drop.
func (t *TabBar) animateReorder(tabs []messages.TabInfo) tea.Cmd {
	m := &t.motion
	if len(tabs) != len(t.tabs) || len(t.dragBounds) != len(tabs) || len(tabs) <= 1 {
		t.cancelDrag()
		return nil
	}
	oldPositions := make(map[string]int, len(t.tabs))
	for i, info := range t.tabs {
		x := t.dragBounds[i].start
		if m.reordering {
			x = m.positions[i].x
		}
		oldPositions[info.SessionID] = x
	}
	for _, info := range tabs {
		if _, ok := oldPositions[info.SessionID]; !ok {
			t.cancelDrag()
			return nil
		}
	}
	offset, left := t.dragOffset, t.dragBounds[t.dragOffset].start
	t.cacheTabBounds(t.motionTabs(tabs), offset, left)
	m.lift(t.dragBounds)
	m.reordering = true
	for i, tab := range tabs {
		p := &m.positions[i]
		p.x = oldPositions[tab.SessionID]
		p.from = p.x
	}
	return m.transition.Start(tabSettleDuration, animation.EaseOutQuint)
}
