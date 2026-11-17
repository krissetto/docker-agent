package sidebar

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// wrapForegroundTodos gives every status a description long enough to wrap at
// the narrow widths below. The first and last carry markdown and backticks,
// which the sidebar paints literally.
func wrapForegroundTodos() []session.Todo {
	return []session.Todo{
		{ID: "pending", Status: "pending", Description: "Refactor the **parser** and `ParseTodos()` helper so every wrapped row keeps its colour"},
		{ID: "progress", Status: "in-progress", Description: "Investigate `go test ./pkg/tui/...` failures across the sidebar and dialog wrap paths"},
		{ID: "done", Status: "completed", Description: "Ship [release notes](https://example.com/notes) and the `--flag` docs before tagging"},
	}
}

// foregroundCell is one visible grapheme and whether a foreground colour is
// active when it is painted.
type foregroundCell struct {
	glyph string
	fg    bool
}

// foregroundCells walks the SGR stream the way a terminal would, tracking only
// whether a foreground colour is currently set.
func foregroundCells(line string) []foregroundCell {
	p := ansi.GetParser()
	defer ansi.PutParser(p)
	var cells []foregroundCell
	var state byte
	fg := false
	for line != "" {
		seq, width, n, next := ansi.DecodeSequence(line, state, p)
		if n == 0 {
			break
		}
		if ansi.HasCsiPrefix(seq) && p.Command() == 'm' {
			fg = applyForeground(fg, p.Params())
		} else if width > 0 {
			cells = append(cells, foregroundCell{glyph: seq, fg: fg})
		}
		state, line = next, line[n:]
	}
	return cells
}

func applyForeground(fg bool, params ansi.Params) bool {
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

// requireTodoTextForeground checks one painted row. Every visible cell that
// belongs to the description must carry a foreground colour; the row must show
// some description text at all, so an empty or fully occluded row fails loudly.
func requireTodoTextForeground(t *testing.T, label, description, row string) {
	t.Helper()
	var text []string
	var uncolored []string
	for _, cell := range foregroundCells(row) {
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

func TestSidebarWrappedTodoTextKeepsForegroundOnEveryLine(t *testing.T) {
	const contentHeight = 60
	items := wrapForegroundTodos()
	for _, width := range []int{16, 22, 30} {
		for _, hovered := range append([]string{""}, todoIDs(items)...) {
			t.Run(fmt.Sprintf("width=%d/hovered=%q", width, hovered), func(t *testing.T) {
				m := newPlacementSidebar(t, false)
				m.SetSize(width, contentHeight)
				_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "session"}, Todos: items})
				settlePlacement(t, m, cmd)
				contentWidth := m.contentWidth(m.cachedNeedsScrollbar)
				indent, _ := rowActions(contentWidth, true)
				if hovered != "" {
					row := requirePlaced(t, m, "todo:"+hovered+":0")
					settlePlacement(t, m, m.updateRegionHover(m.layoutCfg.PaddingLeft+indent+2, int(row.y)-m.scrollview.ScrollOffset()))
					require.Equal(t, "todo:"+hovered+":text", m.hoverTarget)
				}
				var rows []placedRow
				for _, row := range m.placement.rows {
					if strings.HasPrefix(row.id, "todo:") && row.payload != "" {
						rows = append(rows, row)
					}
				}
				require.Len(t, rows, strings.Count(m.todoComp.RenderBody(), "\n")+1, "every wrapped line of every todo is a placed row")
				lines := strings.Split(m.View(), "\n")
				for _, item := range items {
					wrapped := 0
					for _, row := range rows {
						if row.payload != item.ID {
							continue
						}
						wrapped++
						label := fmt.Sprintf("%s row y=%d hovered=%t", item.ID, int(row.y), item.ID == hovered)
						// Painted output, not just the canonical row text.
						y := int(row.y) - m.scrollview.ScrollOffset()
						require.Less(t, y, len(lines))
						painted := ansi.Cut(lines[y], m.layoutCfg.PaddingLeft, m.layoutCfg.PaddingLeft+contentWidth)
						requireTodoTextForeground(t, label, item.Description, painted)
						requireTodoTextForeground(t, label+" placement", item.Description, m.placementText(row, contentWidth))
					}
					if width <= 22 {
						require.Greater(t, wrapped, 1, "%s must wrap at width %d", item.ID, width)
					}
				}
			})
		}
	}
}

func todoIDs(items []session.Todo) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// The checker must distinguish coloured from uncoloured text, or the
// assertions above prove nothing.
func TestTodoForegroundCheckerDetectsUncolouredText(t *testing.T) {
	got := foregroundCells("\x1b[38;2;1;2;3mab\x1b[39m c\x1b[31mE\x1b[0m")
	require.Equal(t, []foregroundCell{{"a", true}, {"b", true}, {" ", false}, {"c", false}, {"E", true}}, got)
}
