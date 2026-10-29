// Package grip renders the resize affordances shared by the chat/input divider
// and the sidebar edge, and owns their shared hover-driven reveal state.
// Both handles are blank at rest and reveal compact centered line segments on
// hover or drag, so divider decoration does not compete with nearby content.
package grip

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	// HorizontalLength is the width of the bright segment on the chat/input divider.
	HorizontalLength = 8
	// VerticalLength is the number of rows reserved for the sidebar edge grip.
	VerticalLength = 3

	horizontalGlyph = "─"
	verticalGlyph   = "│"

	// scrollbarResidual is how much of the adjacent scrollbar's color survives
	// while the vertical grip is fully revealed: a faint ghost keeps the track
	// discoverable without competing with the grip.
	scrollbarResidual = 0.25
)

// Horizontal renders a compact centered divider segment. Visibility controls
// its fade; highlight brightens direct handle hover and active darkens a drag.
// The caller applies outer padding and retains the full-width hit area.
func Horizontal(width int, highlight float64, active bool, visibility float64) string {
	if width <= 0 {
		return ""
	}
	line := strings.Repeat(" ", width)
	if visibility <= 0 {
		return line
	}
	length := min(HorizontalLength, width)
	cell := handleStyle(horizontalGlyph, highlight, active)
	if visibility < 1 {
		cell = styles.FadeLine(cell, visibility)
	}
	left := (width - length) / 2
	return line[:left] + strings.Repeat(cell, length) + line[left+length:]
}

// HorizontalHandleAt reports whether x, relative to the padded divider width,
// falls on the compact segment. The row itself remains a full-width hit target.
func HorizontalHandleAt(width, x int) bool {
	if width <= 0 {
		return false
	}
	length := min(HorizontalLength, width)
	left := (width - length) / 2
	return x >= left && x < left+length
}

// Vertical renders a one-cell-wide column of height rows. At visibility 0 the
// column is plain spaces; otherwise a compact vertical line segment sits at the
// center. Hovering the grip highlights it, while an active drag darkens it.
// Callers may overwrite individual rows (the sidebar keeps its toggle glyph on
// row 0).
func Vertical(height int, highlight float64, active bool, visibility float64) []string {
	if height <= 0 {
		return nil
	}
	lines := make([]string, height)
	for i := range lines {
		lines[i] = " "
	}
	if visibility <= 0 {
		return lines
	}
	top, length := Rows(height)
	cell := handleStyle(verticalGlyph, highlight, active)
	if visibility < 1 {
		cell = styles.FadeLine(cell, visibility)
	}
	for i := top; i < top+length; i++ {
		lines[i] = cell
	}
	return lines
}

func handleStyle(glyph string, highlight float64, active bool) string {
	if active {
		return darkenedResizeStyle().Render(glyph)
	}
	cell := styles.ResizeHandleStyle.Render(glyph)
	if highlight > 0 {
		return styles.HoverText(cell, highlight, styles.Resize)
	}
	return cell
}

func darkenedResizeStyle() lipgloss.Style {
	r, g, b := styles.ColorToRGB(styles.Resize)
	return styles.ResizeHandleStyle.Foreground(styles.RGBToColor(r*0.7, g*0.7, b*0.7))
}

// Rows reports the row range [top, top+length) occupied by the vertical grip.
func Rows(height int) (top, length int) {
	if height <= 0 {
		return 0, 0
	}
	length = min(VerticalLength, height)
	return (height - length) / 2, length
}

// ScrollbarDecorator fades a rendered scrollbar column toward the theme
// background in step with a revealed grip. It returns nil at visibility 0 so
// callers can hand the scrollview its unchanged default path.
func ScrollbarDecorator(visibility float64) scrollview.ScrollbarDecorator {
	if visibility <= 0 {
		return nil
	}
	alpha := 1 - min(visibility, 1)*(1-scrollbarResidual)
	fc := styles.NewFadeContext()
	return func(view string) string {
		if view == "" {
			return view
		}
		lines := strings.Split(view, "\n")
		for i, line := range lines {
			lines[i] = styles.FadeLineCtx(line, alpha, &fc)
		}
		return strings.Join(lines, "\n")
	}
}

// Reveal tracks how visible the vertical grip is. The target is 1 while the
// pointer rests on the edge or a drag is in progress and 0 otherwise; the value
// follows at the sidebar's hover speed and only holds an animation lease while
// it is still moving. Without a runtime the value snaps, which keeps snapshot
// renders and reduced-motion embedders correct.
type Reveal struct {
	sub                              *animation.Subscription
	value, target                    float64
	highlight, highlightTarget       float64
	hovered, handleHovered, dragging bool
}

// NewReveal returns an idle reveal. ar may be nil.
func NewReveal(ar *animation.Runtime) Reveal {
	r := Reveal{}
	if ar != nil {
		sub := ar.Subscribe()
		r.sub = &sub
	}
	return r
}

// SetHovered records whether the pointer is on the edge. The returned command
// is nil; the program owner schedules through its runtime's Continue.
func (r *Reveal) SetHovered(hovered bool) tea.Cmd {
	if r.hovered == hovered {
		return nil
	}
	r.hovered = hovered
	return r.retarget()
}

// SetHandleHovered animates the brighter direct-hover state independently of
// the edge reveal, sharing its animation lease and sidebar hover cadence.
func (r *Reveal) SetHandleHovered(hovered bool) tea.Cmd {
	if r.handleHovered == hovered {
		return nil
	}
	r.handleHovered = hovered
	r.highlightTarget = 0
	if hovered {
		r.highlightTarget = 1
	}
	if r.sub == nil {
		r.highlight = r.highlightTarget
		return nil
	}
	return r.schedule()
}

// SetDragging pins the grip visible for the duration of a drag.
func (r *Reveal) SetDragging(dragging bool) tea.Cmd {
	if r.dragging == dragging {
		return nil
	}
	r.dragging = dragging
	return r.retarget()
}

// Dragging reports whether a drag is pinning the grip visible.
func (r *Reveal) Dragging() bool { return r.dragging }

// Hovered reports whether the pointer rests on the edge.
func (r *Reveal) Hovered() bool { return r.hovered }

// HandleHighlight returns the direct-hover emphasis in [0, 1].
func (r *Reveal) HandleHighlight() float64 { return r.highlight }

// HandleHovered reports whether the pointer is directly over the grip.
func (r *Reveal) HandleHovered() bool { return r.handleHovered }

// Visibility returns the current reveal in [0, 1].
func (r *Reveal) Visibility() float64 { return r.value }

// Animating reports whether the reveal still holds an animation lease.
func (r *Reveal) Animating() bool { return r.sub != nil && r.sub.IsActive() }

// Tick advances the reveal by the accepted tick's elapsed time and reports
// whether the visible value changed, marking the tick dirty when it did.
func (r *Reveal) Tick(tick animation.TickMsg) bool {
	if !r.Animating() {
		return false
	}
	before, after := tick.ElapsedBounds()
	old, oldHighlight := r.value, r.highlight
	delta := after - before
	r.value = animation.HoverStep(r.value, r.target, delta)
	r.highlight = animation.HoverStep(r.highlight, r.highlightTarget, delta)
	if r.value == r.target && r.highlight == r.highlightTarget {
		r.sub.Stop()
	}
	if r.value != old || r.highlight != oldHighlight {
		tick.MarkDirty()
		return true
	}
	return false
}

// Reset drops hover, drag and visibility at once and releases any lease, for
// hidden presentations or a sidebar that leaves the vertical layout.
func (r *Reveal) Reset() {
	r.hovered, r.handleHovered, r.dragging = false, false, false
	r.target, r.value, r.highlight, r.highlightTarget = 0, 0, 0, 0
	if r.sub != nil {
		r.sub.Stop()
	}
}

func (r *Reveal) retarget() tea.Cmd {
	r.target = 0
	if r.hovered || r.dragging {
		r.target = 1
	}
	return r.schedule()
}

func (r *Reveal) schedule() tea.Cmd {
	if r.sub == nil {
		r.value, r.highlight = r.target, r.highlightTarget
		return nil
	}
	if r.value == r.target && r.highlight == r.highlightTarget {
		r.sub.Stop()
		return nil
	}
	return r.sub.Start()
}
