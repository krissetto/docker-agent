package todotool

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tui/components/tab"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// renderCacheCap bounds the per-width render cache. Only a couple of widths are
// ever live at once (the two-pass scrollbar layout renders at two widths), so a
// small cap soaks up a window resize without letting the cache grow unbounded
// between todo updates.
const renderCacheCap = 8

// SidebarComponent represents the todo display component for the sidebar
type SidebarComponent struct {
	todos []todo.Todo
	width int

	// renderCache memoizes the rendered todo list keyed by content width.
	// Render() is called on every animation frame while the agent works (the
	// sidebar rebuilds its whole cache on each spinner tick) and the two-pass
	// scrollbar layout renders at two widths, so without this the O(todos)
	// word-wrap+style work runs many times per second on long lists. Cleared
	// when the todos change (SetTodos) or the theme changes (InvalidateCache).
	renderCache map[int]string
}

func NewSidebarComponent() *SidebarComponent {
	return &SidebarComponent{
		width: 20,
	}
}

func (c *SidebarComponent) SetSize(width int) {
	c.width = width
}

func (c *SidebarComponent) SetTodos(result *tools.ToolCallResult) error {
	if result == nil || result.Meta == nil {
		return nil
	}

	todos, ok := result.Meta.([]todo.Todo)
	if !ok {
		return nil
	}

	c.todos = slices.Clone(todos)
	c.renderCache = nil // todos changed — drop the stale render cache
	return nil
}

// InvalidateCache drops the memoized render. The sidebar calls this when the
// theme changes, since the cached lines embed theme-dependent styling.
func (c *SidebarComponent) InvalidateCache() {
	c.renderCache = nil
}

// Counts returns how many todos are completed and the total count.
func (c *SidebarComponent) Counts() (completed, total int) {
	for _, t := range c.todos {
		if t.Status == "completed" {
			completed++
		}
	}
	return completed, len(c.todos)
}

func (c *SidebarComponent) Render() string {
	body := c.RenderBody()
	if body == "" {
		return ""
	}
	return c.renderTab("TO-DO", body)
}

// RenderBody renders the cached todo rows without section chrome.
func (c *SidebarComponent) RenderBody() string {
	if len(c.todos) == 0 {
		return ""
	}

	if cached, ok := c.renderCache[c.width]; ok {
		return cached
	}

	var lines []string
	for _, todo := range c.todos {
		lines = append(lines, c.renderTodoLine(todo))
	}
	rendered := strings.Join(lines, "\n")

	if c.renderCache == nil || len(c.renderCache) >= renderCacheCap {
		c.renderCache = make(map[int]string, 2)
	}
	c.renderCache[c.width] = rendered
	return rendered
}

func (c *SidebarComponent) renderTodoLine(todoItem todo.Todo) string {
	icon, style := renderTodoIcon(todoItem.Status)

	prefix := "× " + icon + " " + todoItem.Status + " · "
	if lipgloss.Width(prefix) >= c.width {
		return style.Render(ansi.Hardwrap(prefix+todoItem.Description, max(1, c.width), true))
	}
	prefixWidth := lipgloss.Width(prefix)
	maxDescWidth := max(1, c.width-prefixWidth)

	wrapped := toolcommon.WrapLinesWords(todoItem.Description, maxDescWidth)
	indent := strings.Repeat(" ", prefixWidth)

	var b strings.Builder
	for i, line := range wrapped {
		if i == 0 {
			b.WriteString(prefix + line)
		} else {
			b.WriteString("\n" + indent + line)
		}
	}

	return styles.TabPrimaryStyle.Render(style.Render(b.String()))
}

func (c *SidebarComponent) renderTab(title, content string) string {
	return tab.Render(title, content, c.width)
}

// TodoAtLine maps wrapped body rows back to opaque canonical IDs.
func (c *SidebarComponent) TodoAtLine(line int) (todo.Todo, bool) {
	for _, item := range c.todos {
		height := strings.Count(c.renderTodoLine(item), "\n") + 1
		if line >= 0 && line < height {
			return item, true
		}
		line -= height
	}
	return todo.Todo{}, false
}

// ControlsAtLine excludes wrapped continuation and narrow hard-wrapped prefixes.
func (c *SidebarComponent) ControlsAtLine(line int) bool {
	for _, item := range c.todos {
		if line == 0 {
			icon, _ := renderTodoIcon(item.Status)
			return lipgloss.Width("× "+icon+" "+item.Status+" · ") < c.width
		}
		line -= strings.Count(c.renderTodoLine(item), "\n") + 1
		if line < 0 {
			return false
		}
	}
	return false
}
