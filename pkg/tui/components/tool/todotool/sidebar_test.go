package todotool

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func todoResult(descriptions ...string) *tools.ToolCallResult {
	items := make([]todo.Todo, len(descriptions))
	for i, d := range descriptions {
		items[i] = todo.Todo{ID: d, Description: d, Status: "pending"}
	}
	return &tools.ToolCallResult{Meta: items}
}

// TestRenderCacheReflectsTodoChanges guards the render cache added for issue
// #3123: a cache hit must never serve a render from a previous todo list.
func TestRenderCacheReflectsTodoChanges(t *testing.T) {
	t.Parallel()

	c := NewSidebarComponent()
	c.SetSize(40)

	require.NoError(t, c.SetTodos(todoResult("alpha task")))
	first := c.Render()
	assert.Contains(t, first, "alpha")
	assert.Equal(t, first, c.Render(), "same todos + width should hit the cache and return an identical render")

	require.NoError(t, c.SetTodos(todoResult("beta task")))
	second := c.Render()
	assert.Contains(t, second, "beta")
	assert.NotContains(t, second, "alpha", "render must reflect the new todos, not a stale cache")
}

// TestRenderCacheKeyedByWidth verifies the cache distinguishes widths so the
// two-pass scrollbar layout (which renders at two widths) stays correct.
func TestRenderCacheKeyedByWidth(t *testing.T) {
	t.Parallel()

	c := NewSidebarComponent()
	require.NoError(t, c.SetTodos(todoResult("a task with a fairly long description that wraps")))

	c.SetSize(50)
	wide := c.Render()
	c.SetSize(20)
	narrow := c.Render()
	c.SetSize(50)
	wideAgain := c.Render()

	assert.NotEqual(t, wide, narrow, "different widths should produce different renders")
	assert.Equal(t, wide, wideAgain, "returning to a seen width should reuse its cached render")
	// A narrower column wraps into more lines.
	assert.Greater(t, strings.Count(narrow, "\n"), strings.Count(wide, "\n"))
}

// TestInvalidateCacheForcesRecompute verifies theme-change invalidation drops
// the memoized render.
func TestInvalidateCacheForcesRecompute(t *testing.T) {
	t.Parallel()

	c := NewSidebarComponent()
	c.SetSize(40)
	require.NoError(t, c.SetTodos(todoResult("a task")))

	_ = c.Render()
	require.NotEmpty(t, c.renderCache, "render should populate the cache")
	c.InvalidateCache()
	assert.Empty(t, c.renderCache, "InvalidateCache should drop the memoized render")
}

// TestRenderCacheBounded verifies a flood of distinct widths (e.g. a window
// resize drag) cannot grow the cache without bound.
func TestRenderCacheBounded(t *testing.T) {
	t.Parallel()

	c := NewSidebarComponent()
	require.NoError(t, c.SetTodos(todoResult("a task")))
	for w := 20; w < 200; w++ {
		c.SetSize(w)
		_ = c.Render()
	}
	assert.LessOrEqual(t, len(c.renderCache), renderCacheCap)
}

func TestRenderBodyUnheadedWidthAndThemeCache(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	c := NewSidebarComponent()
	c.SetSize(40)
	require.NoError(t, c.SetTodos(todoResult("alpha task")))
	body := c.RenderBody()
	assert.Equal(t, "alpha task", strings.TrimSpace(ansi.Strip(ansi.Cut(body, 2, 40))))
	assert.Equal(t, "○ ", ansi.Strip(ansi.Cut(body, 0, 2)))
	assert.NotContains(t, ansi.Strip(body), "✎")
	assert.NotContains(t, body, "TO-DO")
	assert.Contains(t, c.Render(), "TO-DO")
	assert.Equal(t, body, c.RenderBody())
	c.SetSize(8)
	assert.Greater(t, len(strings.Split(c.RenderBody(), "\n")), 1)
	c.SetSize(40)
	theme := *original
	theme.Colors.TextPrimary = "#abcdef"
	theme.Colors.TextSecondary = "#abcdef"
	styles.ApplyTheme(&theme)
	c.InvalidateCache()
	after := c.RenderBody()
	assert.Equal(t, ansi.Strip(body), ansi.Strip(after))
	assert.NotEqual(t, body, after)
	require.NoError(t, c.SetTodos(todoResult("beta task")))
	assert.Contains(t, c.RenderBody(), "beta")
	assert.NotContains(t, c.RenderBody(), "alpha")
}

func TestTodoGeometryCacheDoesNotWrapForHitsOrUnchangedRows(t *testing.T) {
	c := NewSidebarComponent()
	c.SetSize(32)
	items := make([]todo.Todo, 100)
	for i := range items {
		items[i] = todo.Todo{ID: fmt.Sprint(i), Description: strings.Repeat("long 世界é description ", 4), Status: "completed"}
	}
	require.NoError(t, c.SetTodos(&tools.ToolCallResult{Meta: items}))
	body := c.RenderBody()
	require.Equal(t, uint64(100), c.wraps)
	for range 10 {
		for line := range strings.Count(body, "\n") + 1 {
			item, ok := c.TodoAtLine(line)
			require.True(t, ok)
			require.NotEmpty(t, item.ID)
			c.ControlsAtLine(line)
		}
	}
	require.Equal(t, uint64(100), c.wraps, "hit geometry never renders or wraps")
	items[20].Description = "edited description"
	require.NoError(t, c.SetTodos(&tools.ToolCallResult{Meta: items}))
	c.RenderBody()
	require.Equal(t, uint64(101), c.wraps, "one changed ID replaces only its current row")
	require.NoError(t, c.SetTodos(&tools.ToolCallResult{Meta: items}))
	c.RenderBody()
	require.Equal(t, uint64(101), c.wraps, "identical event does not invalidate")
	slices.Reverse(items)
	require.NoError(t, c.SetTodos(&tools.ToolCallResult{Meta: items}))
	c.RenderBody()
	require.Equal(t, uint64(101), c.wraps, "reordering reuses rows by stable ID")
	c.SetSize(31)
	c.RenderBody()
	require.Equal(t, uint64(201), c.wraps)
	require.Len(t, c.renderCache, 1, "retain only current width")
	require.Len(t, c.rows, 100)
}

func TestCompactTodoRowsShareDescriptionStyleAndGeometry(t *testing.T) {
	for _, status := range []string{"pending", "in-progress", "completed"} {
		for _, selected := range []bool{false, true} {
			rows := RowLines("first words 世界é 👩‍💻\nsecond line", status, 24, selected)
			require.Greater(t, len(rows), 1)
			for i, row := range rows {
				require.LessOrEqual(t, ansi.StringWidth(row), 24)
				description := ansi.Cut(row, 6, 24)
				require.True(t, strings.HasSuffix(description, Description(ansi.Strip(description), status, selected)), "all wrapped description cells carry identical style")
				if i > 0 {
					require.Equal(t, "      ", ansi.Strip(ansi.Cut(row, 0, 6)))
				}
			}
		}
	}
	for width := 1; width < 12; width++ {
		for _, row := range RowLines("界é 👩‍💻 long words", "completed", width, false) {
			require.LessOrEqual(t, ansi.StringWidth(row), width, "width=%d row=%q plain=%q", width, row, ansi.Strip(row))
		}
	}
}

func TestSidebarRightActionsPaintAndHitContract(t *testing.T) {
	for width := 1; width <= 80; width++ {
		for _, withStatus := range []bool{false, true} {
			a := RightActions(width, withStatus)
			for _, first := range []bool{false, true} {
				line := a.Render(Description("界é 👩‍💻 long words", "completed", false), "completed", first)
				require.LessOrEqual(t, ansi.StringWidth(line), width)
				for col := 0; col < width; col++ {
					part := a.PartAt(col, first)
					if part == "text" {
						continue
					}
					glyph := map[string]string{"status": "●", "edit": "✎", "remove": "×"}[part]
					require.Equal(t, glyph, ansi.Strip(ansi.Cut(line, col, col+1)), "width=%d col=%d", width, col)
				}
			}
		}
		rows := sidebarRowLines("first 世界é 👩‍💻\ncontinuation words", "completed", width)
		require.Greater(t, len(rows), 1)
		for i, row := range rows {
			require.LessOrEqual(t, ansi.StringWidth(row), width)
			if i > 0 {
				require.NotContains(t, ansi.Strip(row), "✎")
				require.NotContains(t, ansi.Strip(row), "×")
			}
		}
	}
}

func TestSidebarTodoUsesFullDescriptionWidthAndLeftStatus(t *testing.T) {
	for width := 1; width <= 80; width++ {
		for _, status := range []string{"pending", "in-progress", "completed"} {
			a := SidebarActions(width)
			prefix := width - a.TextWidth
			description := strings.Repeat("x", a.TextWidth) + "\n世界 é 👩‍💻 continuation"
			rows := sidebarRowLines(description, status, width)
			require.Equal(t, width, ansi.StringWidth(rows[0]), "idle description reaches the rightmost cell without a command gutter")
			require.Equal(t, strings.Repeat("x", a.TextWidth), ansi.Strip(ansi.Cut(rows[0], prefix, width)))
			if a.Status >= 0 {
				require.Equal(t, ansi.Strip(StatusIcon(status)), ansi.Strip(ansi.Cut(rows[0], a.Status, a.Status+1)))
				require.Equal(t, "status", a.PartAt(a.Status, true))
			}
			for i, row := range rows {
				require.LessOrEqual(t, ansi.StringWidth(row), width)
				require.NotContains(t, ansi.Strip(row), "✎")
				require.NotContains(t, ansi.Strip(row), "×")
				if i > 0 {
					require.Equal(t, strings.Repeat(" ", prefix), ansi.Strip(ansi.Cut(row, 0, prefix)), "continuation aligns with description, not status")
					for col := range width {
						require.Equal(t, "text", a.PartAt(col, false))
					}
				}
			}
		}
	}
}
