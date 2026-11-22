package dialog

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
)

const dialogResizeDuration = 180 * time.Millisecond

type dialogRect struct{ row, col, width, height int }

type dialogResize struct {
	anim         animation.Transition
	from, target dialogRect
	view         string
	footerRows   int
}

func (r *dialogResize) rect() dialogRect {
	return dialogRect{
		row: r.anim.Lerp(r.from.row, r.target.row), col: r.anim.Lerp(r.from.col, r.target.col),
		width: r.anim.Lerp(r.from.width, r.target.width), height: r.anim.Lerp(r.from.height, r.target.height),
	}
}

func (r *dialogResize) tick(tick animation.TickMsg) {
	if r.anim.Running() {
		r.anim.Tick()
		tick.MarkDirty()
		if !r.anim.Running() {
			r.view = ""
		}
	}
}

func (e *dialogEntry) hitRect() dialogRect {
	if e.resize.anim.Running() {
		return e.resize.rect()
	}
	view := e.dialog.View()
	row, col := e.dialog.Position()
	return dialogRect{row: row, col: col, width: lipgloss.Width(view), height: lipgloss.Height(view)}
}

func (e *dialogEntry) resizeLayer(hovered bool) *lipgloss.Layer {
	rect := e.resize.rect()
	view := resizeDialogFrame(e.resize.view, rect.width, rect.height, e.resize.footerRows)
	return lipgloss.NewLayer(renderClose(view, hovered)).X(rect.col + e.offsetX).Y(rect.row + e.offsetY)
}

func (d *manager) resetResizeTarget(e *dialogEntry) {
	e.resize.target = e.measure(e.dialog.View(), d.width, d.height)
}

func (e *dialogEntry) measure(view string, width, height int) dialogRect {
	row, col := e.dialog.Position()
	return dialogRect{row: row, col: col, width: min(max(0, width), lipgloss.Width(view)), height: min(max(0, height), lipgloss.Height(view))}
}

func (e *dialogEntry) prepareResize() {
	e.resize.view = e.dialog.View()
	e.resize.footerRows = 1
	if footer, ok := e.dialog.(interface{ ResizeFooterRows() int }); ok {
		e.resize.footerRows = footer.ResizeFooterRows()
	}
}

// retargetResize starts at the displayed rectangle, but draws from the latest
// destination so new input is visible immediately, even at the same target size.
func (e *dialogEntry) retargetResize(width, height int) tea.Cmd {
	r := &e.resize
	e.prepareResize()
	target := e.measure(r.view, width, height)
	if target.width == 0 || target.height == 0 || r.target.width == 0 || r.target.height == 0 {
		r.anim.Cancel()
		r.target, r.view = target, ""
		return nil
	}
	if target != r.target {
		from := r.target
		if r.anim.Running() {
			from = r.rect()
		}
		r.from, r.target = from, target
		if from != target {
			return r.anim.Start(dialogResizeDuration, animation.Linear)
		}
		r.anim.Cancel()
	}
	if !r.anim.Running() {
		r.view = ""
	}
	return nil
}

// resizeDialogFrame attaches the live header/body to the moving top and the
// action rows to the moving bottom. Width changes crop/pad rather than reflow.
func resizeDialogFrame(view string, width, height, footerRows int) string {
	source := strings.Split(view, "\n")
	sourceWidth := lipgloss.Width(view)
	bordered := roundedDialog(source, sourceWidth)
	footerRows, topRows := resizeRows(len(source), height, footerRows)
	lines := make([]string, height)
	copy(lines, source[:topRows])
	copy(lines[height-footerRows:], source[len(source)-footerRows:])
	for i, line := range lines {
		if bordered {
			if line == "" {
				line = ansi.Cut(source[1], 0, 1) + strings.Repeat(" ", sourceWidth-2) + ansi.Cut(source[1], sourceWidth-1, sourceWidth)
			}
			line = resizeDialogLine(line, sourceWidth, width, i == 0 || i == height-1, i >= height-footerRows)
		} else {
			line = ansi.Truncate(line, width, "")
			line += strings.Repeat(" ", max(0, width-lipgloss.Width(line)))
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}

func resizeRows(sourceHeight, height, footerRows int) (footer, top int) {
	footer = min(footerRows, sourceHeight, max(0, height-1))
	return footer, min(sourceHeight-footer, height-footer)
}

func roundedDialog(lines []string, width int) bool {
	return width >= 2 && len(lines) > 1 && strings.HasPrefix(ansi.Strip(lines[0]), "╭")
}

func resizeDialogLine(line string, sourceWidth, width int, edge, footer bool) string {
	if width == sourceWidth {
		return line
	}
	if width < 2 {
		return ansi.Truncate(line, width, "")
	}
	left, right := ansi.Cut(line, 0, 1), ansi.Cut(line, sourceWidth-1, sourceWidth)
	inner := ansi.Cut(line, 1, sourceWidth-1)
	if edge {
		inner = strings.Repeat(ansi.Cut(line, 1, 2), width-2)
	} else {
		leading, end, shift := resizeLineShift(inner, sourceWidth, width, footer)
		if shift != 0 {
			inner = strings.Repeat(" ", leading+shift) + ansi.Cut(inner, leading, end)
		}
		inner = ansi.Truncate(inner, width-2, "")
		inner += strings.Repeat(" ", max(0, width-2-lipgloss.Width(inner)))
	}
	return left + inner + right
}

// Centered rows follow the animated width; ordinary body rows stay left-aligned.
func resizeLineShift(inner string, sourceWidth, width int, footer bool) (leading, end, shift int) {
	plain := ansi.Strip(inner)
	leading = len(plain) - len(strings.TrimLeft(plain, " "))
	trailing := len(plain) - len(strings.TrimRight(plain, " "))
	end = lipgloss.Width(inner) - trailing
	difference := width - sourceWidth
	if footer && leading >= 3 && leading-trailing > 4 {
		// The contribution uses right-aligned pills; keep them on the moving edge.
		return leading, end, max(-leading, difference)
	}
	if leading >= 3 && trailing >= 3 && leading-trailing >= -4 && leading-trailing <= 4 {
		if difference < 0 {
			difference-- // Match floor division for negative odd widths.
		}
		shift = max(-leading, difference/2)
	}
	return leading, end, shift
}

// pointer maps the shown body/footer back to the concrete dialog's current
// hitboxes. Blank expansion space must not activate an invisible action.
func (r *dialogResize) pointer(x, y int) (int, int) {
	rect := r.rect()
	x, y = x-rect.col, y-rect.row
	if x < 0 || y < 0 || x >= rect.width || y >= rect.height {
		return -1, -1
	}
	lines := strings.Split(r.view, "\n")
	footer, top := resizeRows(len(lines), rect.height, r.footerRows)
	if y >= rect.height-footer {
		y += len(lines) - rect.height
	} else if y >= top {
		return -1, -1
	}
	width := lipgloss.Width(r.view)
	if width != rect.width && roundedDialog(lines, width) {
		if x == 0 || x == rect.width-1 || y == 0 || y == len(lines)-1 {
			return -1, -1
		}
		leading, end, shift := resizeLineShift(ansi.Cut(lines[y], 1, width-1), width, rect.width, y >= len(lines)-footer)
		if shift != 0 {
			if x-1 < leading+shift || x-1 >= end+shift {
				return -1, -1
			}
			x -= shift
		}
		width-- // The old right border's column can now be blank expansion space.
	}
	if x >= width {
		return -1, -1
	}
	return r.target.col + x, r.target.row + y
}

func clipDialogLayer(layer *lipgloss.Layer, width, height int) *lipgloss.Layer {
	row, col := layer.GetY(), layer.GetX()
	if row >= 0 && col >= 0 && row+layer.Height() <= height && col+layer.Width() <= width {
		return layer
	}
	lines := strings.Split(layer.GetContent(), "\n")
	start, end := max(0, -row), min(len(lines), height-row)
	left, right := max(0, -col), width-col
	if start >= end || left >= right || left >= layer.Width() {
		return lipgloss.NewLayer("")
	}
	lines = lines[start:end]
	for i := range lines {
		lines[i] = ansi.Cut(lines[i], left, right)
	}
	return lipgloss.NewLayer(strings.Join(lines, "\n")).X(max(0, col)).Y(max(0, row))
}
