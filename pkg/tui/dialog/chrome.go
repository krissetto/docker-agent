package dialog

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// The close glyph sits in the frame's upper right padding, not over the title.
const closeGlyph = "✕"

func closeCell(width, height int) (x, y int, ok bool) {
	return width - 3, 1, width >= 4 && height >= 3
}

func renderClose(view string, hovered bool) string {
	lines := strings.Split(view, "\n")
	x, y, ok := closeCell(lipgloss.Width(view), len(lines))
	if !ok {
		return view
	}
	style := styles.NoStyle.Foreground(styles.TextSecondary)
	if hovered {
		style = style.Foreground(styles.Error).Bold(true)
	}
	lines[y] = ansi.Cut(lines[y], 0, x) + style.Render(closeGlyph) + ansi.Cut(lines[y], x+1, lipgloss.Width(lines[y]))
	return strings.Join(lines, "\n")
}

type semanticCloser interface {
	CancelDialogCmd() tea.Cmd
}

func (d *manager) cancelTop() tea.Cmd {
	e := &d.stack[len(d.stack)-1]
	if e.cancelPending {
		return nil
	}
	var cmd tea.Cmd
	if cancel, ok := e.dialog.(semanticCloser); ok {
		cmd = cancel.CancelDialogCmd()
	} else {
		cmd = core.CmdHandler(CloseDialogMsg{})
	}
	e.cancelPending = cmd != nil
	return cmd
}

func (e *dialogEntry) contains(x, y int) (inside, closeHit bool) {
	rect := e.hitRect()
	x, y = x-rect.col-e.offsetX, y-rect.row-e.offsetY
	cx, cy, ok := closeCell(rect.width, rect.height)
	return x >= 0 && x < rect.width && y >= 0 && y < rect.height, ok && x == cx && y == cy
}

const dialogTransitionDuration = 132 * time.Millisecond

// Dismissed cards are snapshots: the logical stack changes immediately so
// existing response sequences and tab routing don't wait for a visual effect.
type closingFrame struct {
	view     string
	row, col int
	anim     animation.Transition
	from     float64
}

func animatedLayer(view string, row, col int, alpha float64) *lipgloss.Layer {
	lines := strings.Split(view, "\n")
	if alpha < 1 {
		height := max(1, int(float64(len(lines))*alpha+0.5))
		top := (len(lines) - height) / 2
		row += top
		lines = lines[top : top+height]
		palette := styles.CurrentFadePalette()
		for i, line := range lines {
			lines[i] = palette.FadeLine(line, alpha)
		}
	}
	return lipgloss.NewLayer(strings.Join(lines, "\n")).X(col).Y(row)
}

func (d *manager) updateChromeAnimation(tick animation.TickMsg) {
	for i := range d.stack {
		entry := &d.stack[i]
		if entry.opening.Running() {
			entry.opening.Tick()
			if !entry.opening.Running() {
				d.resetResizeTarget(entry)
			}
			tick.MarkDirty()
		}
		entry.resize.tick(tick)
	}
	if d.closing != nil {
		d.closing.anim.Tick()
		if !d.closing.anim.Running() {
			d.closing = nil
		}
		tick.MarkDirty()
	}
}

func (d *manager) stopClosing() {
	if d.closing != nil {
		d.closing.anim.Cancel()
		d.closing = nil
	}
}

func (d *manager) closeWithAnimation() (layout.Model, tea.Cmd) {
	if d.clock == nil || d.width <= 0 || d.height <= 0 || len(d.stack) == 0 {
		return d.handleClose()
	}
	d.stopClosing()
	e := &d.stack[len(d.stack)-1]
	var layer *lipgloss.Layer
	if e.resize.anim.Running() {
		layer = clipDialogLayer(e.resizeLayer(false), d.width, d.height)
	} else {
		row, col := e.dialog.Position()
		layer = lipgloss.NewLayer(renderClose(e.dialog.View(), false)).X(col + e.offsetX).Y(row + e.offsetY)
	}
	from := 1.0
	if e.opening.Running() {
		from = e.opening.Value()
	}
	_, cmd := d.handleClose()
	if from == 0 || layer.GetContent() == "" {
		return d, cmd
	}
	frame := &closingFrame{view: layer.GetContent(), row: layer.GetY(), col: layer.GetX(), from: from, anim: animation.NewTransition(d.clock)}
	d.closing = frame
	return d, tea.Batch(cmd, frame.anim.Start(dialogTransitionDuration, animation.EaseOutCubic))
}
