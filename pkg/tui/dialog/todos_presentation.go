package dialog

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type todoRowGeometry struct {
	start, end, width              int
	remove, statusStart, statusEnd int
}

type todoHover struct {
	value, from, target float64
	elapsed             time.Duration
}

func (d *todosDialog) prepareRows(width int) {
	d.lines = newGroupedList()
	d.prepared = nil
	d.lineRows = nil
	add := func(text string, owner int, first bool) {
		if first {
			d.lines.AddItem(text)
		} else {
			d.lines.AddNonItem(text)
		}
		d.lineRows = append(d.lineRows, owner)
	}
	for i, todo := range d.todos {
		if i > 0 {
			add("", -1, false)
		}
		g := todoRowGeometry{start: len(d.lineRows), width: width, remove: -1, statusStart: -1, statusEnd: -1}
		lead := "  "
		if i == d.selected {
			lead = styles.HighlightWhiteStyle.Render("› ")
		}
		hover := d.hover[todo.ID].value
		status := todotool.StatusLabel(todo.Status)
		statusWidth := max(13, ansi.StringWidth(status))
		prefix := lead + styles.MutedStyle.Render("× ") + status + strings.Repeat(" ", statusWidth-ansi.StringWidth(status)+2)
		indent := ansi.StringWidth(prefix)
		description := cleanDetail(todo.Description)
		if width-indent >= 12 {
			g.remove, g.statusStart, g.statusEnd = 2, 4, 4+ansi.StringWidth(status)
			wrapped := strings.Split(ansi.Wrap(description, width-indent, ""), "\n")
			add(lead+styles.HoverText(strings.TrimPrefix(prefix, lead)+wrapped[0], hover, styles.TextPrimary), i, true)
			for _, line := range wrapped[1:] {
				add(strings.Repeat(" ", indent)+styles.HoverText(line, hover, styles.TextPrimary), i, false)
			}
		} else {
			// Compact rows give the description its own width; controls never overlap it.
			control := lead
			if width >= 5 {
				control += styles.MutedStyle.Render("× ")
				g.remove, g.statusStart = 2, 4
			} else {
				control = ""
			}
			controlWidth := ansi.StringWidth(control)
			wrapped := strings.Split(ansi.Wrap(status, max(1, width-controlWidth), ""), "\n")
			if g.statusStart >= 0 {
				g.statusEnd = min(width, g.statusStart+ansi.StringWidth(wrapped[0]))
			}
			add(control+styles.HoverText(wrapped[0], hover, styles.TextPrimary), i, true)
			for _, line := range wrapped[1:] {
				add(strings.Repeat(" ", controlWidth)+styles.HoverText(line, hover, styles.TextPrimary), i, false)
			}
			indent = 0
			if width >= 4 {
				indent = 2
			}
			for _, line := range strings.Split(ansi.Wrap(description, max(1, width-indent), ""), "\n") {
				add(strings.Repeat(" ", indent)+styles.HoverText(line, hover, styles.TextPrimary), i, false)
			}
		}
		g.end = len(d.lineRows)
		d.prepared = append(d.prepared, g)
	}
}

// Manager coordinates are already drag-adjusted; translate to body cells once.
func (d *todosDialog) hitAt(x, y int) (index, line, col int) {
	col, line = x-d.bodyX, y-d.bodyY
	if d.Height() < 5 || col < 0 || col >= d.bodyWidth-d.scrollview.ReservedCols() || line < 0 || line >= d.bodyHeight || y >= d.Height()-2 {
		return -1, -1, -1
	}
	line += d.scrollview.ScrollOffset()
	if line < 0 || line >= len(d.lineRows) {
		return -1, -1, -1
	}
	index = d.lineRows[line]
	if index < 0 || index >= len(d.prepared) || col >= d.prepared[index].width {
		return -1, -1, -1
	}
	return index, line, col
}

func (d *todosDialog) BindAnimationRuntime(runtime *animation.Runtime) {
	d.hoverAnimation.SetRuntime(runtime)
	d.animationBound = true
}

func (d *todosDialog) syncHover() {
	id := ""
	if d.pointerKnown {
		if i, _, _ := d.hitAt(d.pointerX, d.pointerY); i >= 0 {
			id = d.todos[i].ID
		}
	}
	if id == d.hovered {
		return
	}
	d.hovered = id
	if d.hover == nil {
		d.hover = make(map[string]todoHover)
	}
	if id != "" {
		if _, ok := d.hover[id]; !ok {
			d.hover[id] = todoHover{}
		}
	}
	running := false
	for key, state := range d.hover {
		target := 0.0
		if key == id {
			target = 1
		}
		if state.target != target {
			state.from, state.target, state.elapsed = state.value, target, 0
			if !d.animationBound {
				state.value = target
			}
		}
		running = running || state.value != state.target
		if state.value == 0 && state.target == 0 {
			delete(d.hover, key)
		} else {
			d.hover[key] = state
		}
	}
	if running {
		d.hoverAnimation.Start()
	} else {
		d.hoverAnimation.Stop()
	}
	d.MarkVisualDirty()
}

func (d *todosDialog) tickHover(tick animation.TickMsg) {
	if !d.hoverAnimation.IsActive() {
		return
	}
	before, after := tick.ElapsedBounds()
	running, changed := false, false
	for id, state := range d.hover {
		old := state.value
		state.elapsed += after - before
		p := min(1, float64(state.elapsed)/float64(animation.ShortDuration))
		state.value = state.from + (state.target-state.from)*animation.EaseOutCubic(p)
		changed = changed || old != state.value
		running = running || state.value != state.target
		if state.value == 0 && state.target == 0 {
			delete(d.hover, id)
		} else {
			d.hover[id] = state
		}
	}
	if !running {
		d.hoverAnimation.Stop()
	}
	if changed {
		d.MarkVisualDirty()
		tick.MarkDirty()
	}
}

func (d *todosDialog) pruneHover() {
	live := make(map[string]bool, len(d.todos))
	for _, todo := range d.todos {
		live[todo.ID] = true
	}
	for id := range d.hover {
		if !live[id] {
			delete(d.hover, id)
		}
	}
	if !live[d.hovered] {
		d.hovered = ""
	}
	if len(d.hover) == 0 {
		d.hoverAnimation.Stop()
	}
}

func (d *todosDialog) StopAnimations() {
	d.hoverAnimation.Stop()
	d.hover, d.hovered, d.pointerKnown = nil, "", false
	d.MarkVisualDirty()
}

func (d *todosDialog) Cleanup() { d.StopAnimations() }
