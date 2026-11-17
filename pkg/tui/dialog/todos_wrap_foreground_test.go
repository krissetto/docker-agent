package dialog

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// wrapForegroundDialogTodos gives every status a description that wraps at the
// narrow dialog widths below. The first and last carry markdown and backticks.
func wrapForegroundDialogTodos() []session.Todo {
	return []session.Todo{
		{ID: "pending", Status: "pending", Description: "Refactor the **parser** and `ParseTodos()` helper so every wrapped row keeps its colour"},
		{ID: "progress", Status: "in-progress", Description: "Investigate `go test ./pkg/tui/...` failures across the sidebar and dialog wrap paths"},
		{ID: "done", Status: "completed", Description: "Ship [release notes](https://example.com/notes) and the `--flag` docs before tagging"},
	}
}

// todoForegroundCell is one visible grapheme and whether a foreground colour is
// active when it is painted.
type todoForegroundCell struct {
	glyph string
	fg    bool
}

// todoForegroundCells walks the SGR stream the way a terminal would, tracking
// only whether a foreground colour is currently set.
func todoForegroundCells(line string) []todoForegroundCell {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var cells []todoForegroundCell
	var state byte
	fg := false
	for line != "" {
		seq, width, n, next := ansi.DecodeSequence(line, state, p)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			fg = applyTodoForeground(fg, p.Params())
		} else if width > 0 {
			cells = append(cells, todoForegroundCell{glyph: seq, fg: fg})
		}
		state, line = next, line[n:]
	}
	return cells
}

func applyTodoForeground(fg bool, params ansi.Params) bool {
	if len(params) == 0 {
		return false
	}
	for i := 0; i < len(params); i++ {
		switch param := params[i].Param(0); {
		case param == 0 || param == 39:
			fg = false
		case (param >= 30 && param <= 37) || (param >= 90 && param <= 97):
			fg = true
		case param == 38:
			var c color.Color
			if consumed := ansi.ReadStyleColor(params[i:], &c); consumed > 0 {
				fg = true
				i += consumed - 1
			}
		}
	}
	return fg
}

// requireTodoRowForeground checks one painted dialog row: the row must show
// description text, and every visible description glyph must carry a foreground.
func requireTodoRowForeground(t *testing.T, label, description, row string) {
	t.Helper()
	var text, uncolored []string
	for _, cell := range todoForegroundCells(row) {
		if strings.TrimSpace(cell.glyph) == "" || !strings.Contains(description, cell.glyph) {
			continue
		}
		text = append(text, cell.glyph)
		if !cell.fg {
			uncolored = append(uncolored, cell.glyph)
		}
	}
	require.NotEmpty(t, text, "%s: row paints no description text: %q", label, ansi.Strip(row))
	require.Empty(t, uncolored, "%s: description glyphs without foreground SGR in %q", label, ansi.Strip(row))
}

func TestTodosDialogWrappedTextKeepsForegroundOnEveryLine(t *testing.T) {
	items := wrapForegroundDialogTodos()
	for _, width := range []int{24, 40} {
		for _, selected := range items {
			for _, hovered := range append([]string{""}, todoDialogIDs(items)...) {
				t.Run(fmt.Sprintf("width=%d/selected=%s/hovered=%q", width, selected.ID, hovered), func(t *testing.T) {
					d := NewTodosDialog(messages.TodoScope{SessionID: "session"}, items, selected.ID).(*todosDialog)
					d.SetSize(width, 40)
					if hovered != "" {
						row := -1
						for i, todo := range d.todos {
							if todo.ID == hovered {
								row = i
							}
						}
						require.GreaterOrEqual(t, row, 0)
						d.Update(tea.MouseMotionMsg{X: d.bodyX + 4, Y: d.bodyY + d.prepared[row].start - d.scrollview.ScrollOffset()})
						require.Equal(t, hovered, d.hovered)
						require.Greater(t, d.hover[hovered].value, 0.0)
					}
					// Pointer coordinates are screen-relative; View lines are dialog-relative.
					top, _ := d.Position()
					bodyTop := d.bodyY - top
					textWidth := d.bodyWidth - d.scrollview.ReservedCols()
					for i, todo := range d.todos {
						g := d.prepared[i]
						wrapped := 0
						for line := g.start; line < g.end; line++ {
							wrapped++
							// Only painted rows can be checked, so scroll each wrapped line into the viewport first.
							d.scrollview.EnsureLineVisible(line)
							offset := d.scrollview.ScrollOffset()
							require.GreaterOrEqual(t, line, offset)
							require.Less(t, line, offset+d.scrollview.VisibleHeight())
							lines := strings.Split(d.View(), "\n")
							painted := ansi.Cut(lines[bodyTop+line-offset], d.bodyX, d.bodyX+textWidth)
							requireTodoRowForeground(t, fmt.Sprintf("%s line %d hovered=%t", todo.ID, line, todo.ID == hovered), todo.Description, painted)
						}
						if width <= 24 {
							require.Greater(t, wrapped, 1, "%s must wrap at width %d", todo.ID, width)
						}
					}
				})
			}
		}
	}
}

func todoDialogIDs(items []session.Todo) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// The checker must distinguish coloured from uncoloured text, or the assertions above prove nothing.
func TestTodoDialogForegroundCheckerDetectsUncolouredText(t *testing.T) {
	got := todoForegroundCells("\x1b[38;2;1;2;3mab\x1b[39m c\x1b[31mE\x1b[0m")
	require.Equal(t, []todoForegroundCell{{"a", true}, {"b", true}, {" ", false}, {"c", false}, {"E", true}}, got)
}
