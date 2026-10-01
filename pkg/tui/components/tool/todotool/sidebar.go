package todotool

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tui/components/tab"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const renderCacheCap = 1

type cachedTodo struct {
	item todo.Todo
	rows []string
}

// SidebarComponent retains one current layout, with geometry produced alongside text.
type SidebarComponent struct {
	todos       []todo.Todo
	width       int
	completed   int
	renderCache map[int]string
	rows        []cachedTodo
	lineOwners  []int
	lineOffsets []int
	ids         map[string]int
	theme       uint64
	wraps       uint64
}

func NewSidebarComponent() *SidebarComponent { return &SidebarComponent{width: 20} }
func (c *SidebarComponent) SetSize(width int) {
	if c.width != width {
		c.width = max(1, width)
		c.InvalidateCache()
	}
}

func (c *SidebarComponent) SetTodos(result *tools.ToolCallResult) error {
	if result == nil || result.Meta == nil {
		return nil
	}
	items, ok := result.Meta.([]todo.Todo)
	if !ok {
		return nil
	}
	if slices.Equal(c.todos, items) {
		return nil
	}
	c.todos = slices.Clone(items)
	c.completed = 0
	for _, item := range items {
		if item.Status == "completed" {
			c.completed++
		}
	}
	c.renderCache = nil
	return nil
}
func (c *SidebarComponent) InvalidateCache()   { c.renderCache = nil; c.rows = nil }
func (c *SidebarComponent) Counts() (int, int) { return c.completed, len(c.todos) }
func (c *SidebarComponent) Render() string {
	body := c.RenderBody()
	if body == "" {
		return ""
	}
	return tab.Render("TO-DO", body, c.width)
}

func (c *SidebarComponent) RenderBody() string {
	if c.theme != styles.ThemeGeneration() {
		c.InvalidateCache()
		c.theme = styles.ThemeGeneration()
	}
	if text, ok := c.renderCache[c.width]; ok {
		return text
	}
	previous := make(map[string]cachedTodo, len(c.rows))
	for _, row := range c.rows {
		previous[row.item.ID] = row
	}
	c.rows = make([]cachedTodo, len(c.todos))
	c.lineOwners = nil
	c.lineOffsets = nil
	c.ids = make(map[string]int, len(c.todos))
	var lines []string
	for i, item := range c.todos {
		cached, ok := previous[item.ID]
		if !ok || cached.item != item {
			cached = cachedTodo{item: item, rows: sidebarRowLines(item.Description, item.Status, c.width)}
			c.wraps++
		}
		c.rows[i] = cached
		c.ids[item.ID] = i
		for offset, line := range cached.rows {
			lines = append(lines, line)
			c.lineOwners = append(c.lineOwners, i)
			c.lineOffsets = append(c.lineOffsets, offset)
		}
	}
	text := strings.Join(lines, "\n")
	c.renderCache = map[int]string{c.width: text}
	return text
}

func (c *SidebarComponent) TodoAtLine(line int) (todo.Todo, bool) {
	c.RenderBody()
	if line < 0 || line >= len(c.lineOwners) {
		return todo.Todo{}, false
	}
	return c.todos[c.lineOwners[line]], true
}

func (c *SidebarComponent) TodoByID(id string) (todo.Todo, bool) {
	c.RenderBody()
	i, ok := c.ids[id]
	if !ok {
		return todo.Todo{}, false
	}
	return c.todos[i], true
}

func (c *SidebarComponent) ControlsAtLine(line int) bool {
	c.RenderBody()
	return line >= 0 && line < len(c.lineOffsets) && c.lineOffsets[line] == 0
}

func (c *SidebarComponent) OffsetAtLine(line int) int {
	c.RenderBody()
	if line < 0 || line >= len(c.lineOffsets) {
		return -1
	}
	return c.lineOffsets[line]
}
