package dialog

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type todosDialog struct {
	lines *groupedList
	pickerCore
	scope              messages.TodoScope
	todos              []session.Todo
	busy               bool
	removeArmed        string
	errorText          string
	prepared           []todoRowGeometry
	lineRows           []int
	hover              map[string]todoHover
	hovered            string
	hoverAnimation     animation.Subscription
	animationBound     bool
	pointerX, pointerY int
	pointerKnown       bool
}

func NewTodosDialog(scope messages.TodoScope, todos []session.Todo, selected string) Dialog {
	d := &todosDialog{pickerCore: newPickerCore(pickerLayout{WidthPercent: 85, MinWidth: 36, MaxWidth: 120, HeightPercent: 85, MaxHeight: 40, ListOverhead: 8}, ""), scope: scope, todos: slices.Clone(todos)}
	d.textInput.Blur()
	for i, todo := range todos {
		if todo.ID == selected {
			d.selected = i
		}
	}
	return d
}
func (d *todosDialog) Init() tea.Cmd { return nil }
func (d *todosDialog) edit(status string, remove bool) tea.Cmd {
	if d.busy || d.selected < 0 || d.selected >= len(d.todos) {
		return nil
	}
	id := d.todos[d.selected].ID
	if remove && d.removeArmed != id {
		d.removeArmed = id
		return nil
	}
	d.removeArmed = ""
	d.busy = true
	d.errorText = ""
	return core.CmdHandler(messages.EditTodoMsg{Scope: d.scope, ID: id, Status: status, Remove: remove})
}
func (d *todosDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	defer func() {
		if preparesDialogBody(msg) {
			d.renderBody(true)
		}
		d.syncHover()
	}()
	switch msg := msg.(type) {
	case animation.TickMsg:
		d.tickHover(msg)
	case tea.MouseMotionMsg:
		d.pointerX, d.pointerY, d.pointerKnown = msg.X, msg.Y, true
		_, cmd := d.scrollview.Update(msg)
		return d, cmd
	case tea.MouseWheelMsg:
		d.pointerX, d.pointerY, d.pointerKnown = msg.X, msg.Y, true
		_, cmd := d.scrollview.Update(msg)
		return d, cmd
	case messages.WheelCoalescedMsg:
		d.pointerX, d.pointerY, d.pointerKnown = msg.X, msg.Y, true
		_, cmd := d.scrollview.Update(msg)
		return d, cmd
	case messages.TodosSnapshotMsg:
		if msg.Scope != d.scope {
			return d, nil
		}
		d.busy = false
		if msg.Err != nil {
			d.errorText = msg.Err.Error()
			return d, nil
		}
		id := ""
		if len(d.todos) > 0 {
			id = d.todos[d.selected].ID
		}
		d.todos = slices.Clone(msg.Todos)
		d.selected = min(d.selected, max(0, len(d.todos)-1))
		d.removeArmed = ""
		for i, todo := range d.todos {
			if todo.ID == id {
				d.selected = i
			}
		}
		d.pruneHover()
		d.renderBody(true)
		d.scrollview.EnsureLineVisible(d.selectedLine())
	case tea.WindowSizeMsg:
		return d, d.SetSize(msg.Width, msg.Height)
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc":
			if d.removeArmed != "" {
				d.removeArmed = ""
				return d, nil
			}
			return d, closeDialogCmd()
		case "q", "ctrl+c":
			return d, closeDialogCmd()
		case "up", "k":
			d.navigate(-1, len(d.todos), d.selectedLine)
			d.removeArmed = ""
		case "down", "j":
			d.navigate(1, len(d.todos), d.selectedLine)
			d.removeArmed = ""
		case "home":
			d.removeArmed = ""
			d.navigate(-d.selected, len(d.todos), d.selectedLine)
		case "end":
			d.removeArmed = ""
			d.navigate(len(d.todos)-1-d.selected, len(d.todos), d.selectedLine)
		case "pgup":
			d.scrollview.ScrollBy(-d.scrollview.VisibleHeight())
		case "pgdown":
			d.scrollview.ScrollBy(d.scrollview.VisibleHeight())
		case "p", "1":
			return d, d.edit("pending", false)
		case "i", "2":
			return d, d.edit("in-progress", false)
		case "c", "3":
			return d, d.edit("completed", false)
		case "enter", "space":
			if len(d.todos) > 0 {
				return d, d.edit(todotool.NextStatus(d.todos[d.selected].Status), false)
			}
		case "delete", "backspace", "d":
			return d, d.edit("", true)
		}
	case tea.MouseClickMsg:
		if handled, cmd := d.scrollview.Update(msg); handled {
			return d, cmd
		}
		if msg.Button != tea.MouseLeft {
			return d, nil
		}
		d.pointerX, d.pointerY, d.pointerKnown = msg.X, msg.Y, true
		index, line, col := d.hitAt(msg.X, msg.Y)
		if index < 0 {
			return d, nil
		}
		if d.selected != index {
			d.removeArmed = ""
		}
		d.selected = index
		g := d.prepared[index]
		if line == g.start {
			if col == g.remove {
				return d, d.edit("", true)
			}
			if col >= g.statusStart && col < g.statusEnd {
				return d, d.edit(todotool.NextStatus(d.todos[index].Status), false)
			}
		}
	default:
		if handled, cmd := d.scrollview.Update(msg); handled {
			return d, cmd
		}
	}
	return d, nil
}
func (d *todosDialog) SetSize(w, h int) tea.Cmd {
	cmd := d.pickerCore.SetSize(w, h)
	d.renderBody(true)
	d.scrollview.EnsureLineVisible(d.selectedLine())
	d.syncHover()
	return cmd
}
func (d *todosDialog) Position() (int, int) { return d.CenterDialog(d.View()) }
func (d *todosDialog) View() string         { return d.renderBody(false) }
func (d *todosDialog) renderBody(prepare bool) string {
	width, _, inner := d.dialogSize()
	header := RenderTitle("Todos", inner, styles.DialogTitleStyle)
	_, _, bodyWidth, _ := d.bodyFrame(styles.DialogStyle, width)
	inner = max(1, bodyWidth-d.scrollview.ReservedCols())
	d.prepareRows(inner)
	lines := d.lines.Lines()
	if len(lines) == 0 {
		lines = []string{styles.MutedStyle.Render("No todos in this scope.")}
	}
	hint := "↑↓ choose · Space cycle · p/i/c status · PgUp/Dn scroll · d/× remove · Esc close"
	if d.removeArmed != "" {
		hint = "Press d / click × again to remove · Esc cancel"
	} else if d.busy {
		hint = "Saving…"
	} else if d.errorText != "" {
		hint = d.errorText
	}
	footer := styles.MutedStyle.Render(ansi.Truncate(hint, inner, ""))
	if prepare {
		d.PrepareScrollableBody(styles.DialogStyle, width, header, strings.Join(lines, "\n"), footer)
		return ""
	}
	return d.RenderScrollableBody(styles.DialogStyle, width, header, strings.Join(lines, "\n"), footer)
}

func (d *todosDialog) selectedLine() int {
	if d.selected >= 0 && d.selected < len(d.prepared) {
		return d.prepared[d.selected].start
	}
	return 0
}

// Let Escape reach Update while a removal confirmation is armed.
func (d *todosDialog) DialogClosable() bool { return d.removeArmed == "" }
