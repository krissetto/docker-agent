package chat

import (
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/grip"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
)

// gripDim names the neighbour whose scrollbar is currently dimmed beside a
// revealed sidebar edge grip.
type gripDim int

const (
	gripDimNone     gripDim = iota
	gripDimMessages         // right dock: the transcript scrollbar touches the edge
	gripDimSidebar          // left dock: the sidebar scrollbar touches the edge
)

// sidebarGripState is the hover/drag reveal of the compact grip on the
// sidebar edge column. Hover and drag changes only register animation work;
// the root schedules ticks through its runtime's Continue.
type sidebarGripState struct {
	reveal grip.Reveal
	dimmed gripDim
}

func newSidebarGripState(ar *animation.Runtime) sidebarGripState {
	return sidebarGripState{reveal: grip.NewReveal(ar)}
}

// ClearSidebarEdgeHover ends the edge grip hover when the pointer leaves the
// content region without crossing this page again.
func ClearSidebarEdgeHover(page Page) {
	if p, ok := page.(*chatPage); ok {
		p.setSidebarGripHover(false, false)
		p.messages.ClearPointerHover()
	}
}

// sidebarGripAt reports whether (x, y) is on the sidebar edge column: the
// resize handle rows or the toggle glyph that shares the column.
func (p *chatPage) sidebarGripAt(x, y int) bool {
	if !p.sidebarInteractive() {
		return false
	}
	switch NewHitTest(p).At(x, y) {
	case TargetSidebarResizeHandle, TargetSidebarToggle:
		return true
	}
	return false
}

// sidebarGripHandleAt reports whether the pointer rests on the compact grip's
// visible line segment, rather than elsewhere in the edge column.
func (p *chatPage) sidebarGripHandleAt(x, y int) bool {
	sl := p.computeSidebarLayout()
	if !p.sidebarGripAt(x, y) || sl.mode != sidebarVertical {
		return false
	}
	height, originY := max(sl.chatHeight, p.sidebarPresentationExtent), p.mainContentInset
	if g := p.splitPresentation; g != nil {
		height, originY = g.Shell.SidebarHandle.Height, g.Shell.SidebarHandle.Y
	}
	top, length := grip.Rows(height)
	return y >= originY+top && y < originY+top+length
}

func (p *chatPage) setSidebarGripHover(edge, handle bool) {
	before, highlight := p.sidebarGrip.reveal.Visibility(), p.sidebarGrip.reveal.HandleHighlight()
	p.sidebarGrip.reveal.SetHovered(edge)
	p.sidebarGrip.reveal.SetHandleHovered(handle)
	if p.sidebarGrip.reveal.Visibility() != before || p.sidebarGrip.reveal.HandleHighlight() != highlight {
		p.applySidebarGrip()
	}
}

func (p *chatPage) setSidebarGripDragging(dragging bool) {
	if p.sidebarGrip.reveal.Dragging() == dragging {
		return
	}
	p.sidebarGrip.reveal.SetDragging(dragging)
	// The active color applies immediately even when visibility is unchanged.
	p.applySidebarGrip()
}

// tickSidebarGrip advances the reveal on an accepted tick; settled reveals
// hold no lease, so this is free outside a fade.
func (p *chatPage) tickSidebarGrip(tick animation.TickMsg) {
	if p.sidebarGrip.reveal.Tick(tick) {
		p.applySidebarGrip()
	}
}

// resetSidebarGrip hides the grip at once and restores any dimmed scrollbar,
// for hidden presentations and layouts without an edge column.
func (p *chatPage) resetSidebarGrip() {
	r := &p.sidebarGrip.reveal
	if r.Visibility() == 0 && !r.Hovered() && !r.HandleHovered() && !r.Dragging() && p.sidebarGrip.dimmed == gripDimNone {
		return
	}
	r.Reset()
	p.applySidebarGrip()
}

// syncSidebarGrip follows layout changes: a dock swap moves the dimmed
// scrollbar to the new neighbour and leaving the vertical layout hides the grip.
func (p *chatPage) syncSidebarGrip() {
	if p.computeSidebarLayout().mode != sidebarVertical {
		p.resetSidebarGrip()
		return
	}
	if p.sidebarGrip.dimmed != p.sidebarGripDimTarget() {
		p.applySidebarGrip()
	}
}

func (p *chatPage) sidebarGripDimTarget() gripDim {
	if p.sidebarGrip.reveal.Visibility() <= 0 || !p.sidebarInteractive() {
		return gripDimNone
	}
	sl := p.computeSidebarLayout()
	switch {
	case sl.mode != sidebarVertical:
		return gripDimNone
	case sl.sidebarOnLeft:
		return gripDimSidebar
	default:
		return gripDimMessages
	}
}

// applySidebarGrip repaints the edge column and dims the adjacent scrollbar in
// step with the reveal. Only final composition caches move: the shell
// generation and the neighbour's scrollbar paint, never transcript or sidebar
// content.
func (p *chatPage) applySidebarGrip() {
	p.shellVisualGeneration++
	target := p.sidebarGripDimTarget()
	if previous := p.sidebarGrip.dimmed; previous != target && previous != gripDimNone {
		p.setGripScrollbarDecorator(previous, nil)
	}
	if target != gripDimNone {
		p.setGripScrollbarDecorator(target, grip.ScrollbarDecorator(p.sidebarGrip.reveal.Visibility()))
	}
	p.sidebarGrip.dimmed = target
}

func (p *chatPage) setGripScrollbarDecorator(dim gripDim, decorator scrollview.ScrollbarDecorator) {
	switch dim {
	case gripDimMessages:
		p.messages.SetScrollbarDecorator(decorator)
	case gripDimSidebar:
		p.sidebar.SetScrollbarDecorator(decorator)
	}
}

// sidebarHandleLines renders the edge column with the current reveal; row 0
// is overwritten by the caller's toggle glyph.
func (p *chatPage) sidebarHandleLines(height int) []string {
	return grip.Vertical(height, p.sidebarGrip.reveal.HandleHighlight(), p.sidebarGrip.reveal.Dragging(), p.sidebarGrip.reveal.Visibility())
}
