package dialog

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type todoRowGeometry struct {
	start, end, width                    int
	remove, statusStart, statusEnd, edit int
}

type todoHover struct {
	value, target float64
}

type cachedDialogTodo struct {
	item     session.Todo
	selected bool
	rows     []string
}

func (d *todosDialog) prepareRows(width int) {
	theme := styles.ThemeGeneration()
	if d.preparedWidth == width && d.preparedTheme == theme && !d.rowsDirty && d.preparedSelected == d.selected {
		return
	}
	if d.preparedWidth != width || d.preparedTheme != theme {
		d.rowCache = nil
	}
	d.preparedWidth, d.preparedTheme, d.preparedSelected, d.rowsDirty = width, theme, d.selected, false
	previous := d.rowCache
	d.rowCache = make(map[string]cachedDialogTodo, len(d.todos))
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
		g := todoRowGeometry{start: len(d.lineRows), width: width, remove: -1, statusStart: -1, statusEnd: -1, edit: -1}
		gutter := min(2, max(0, width-1))
		if width-gutter >= 8 {
			g.statusStart, g.statusEnd, g.remove, g.edit = gutter, gutter+1, gutter+2, gutter+4
		}
		selected := i == d.selected
		cached, ok := previous[todo.ID]
		if !ok || cached.item != todo || cached.selected != selected {
			cached = cachedDialogTodo{item: todo, selected: selected, rows: todotool.RowLines(ansi.Strip(strings.ReplaceAll(todo.Description, "\r", "")), todo.Status, max(1, width-gutter), selected)}
		}
		d.rowCache[todo.ID] = cached
		for line, text := range cached.rows {
			lead := strings.Repeat(" ", gutter)
			if selected && line == 0 && gutter == 2 {
				lead = styles.HighlightWhiteStyle.Render("› ")
			}
			add(lead+text, i, line == 0)
		}
		g.end = len(d.lineRows)
		d.prepared = append(d.prepared, g)
	}
	d.preparedBody = strings.Join(d.lines.Lines(), "\n")
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
			state.target = target
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
		state.value = animation.HoverStep(state.value, state.target, after-before)
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
