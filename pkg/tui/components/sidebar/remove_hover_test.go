package sidebar

import (
	"fmt"
	"image/color"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestRemoveHoverUsesErrorColorWithoutChangingItemText(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	for _, themeName := range []string{"default", "default-light"} {
		theme, err := styles.LoadTheme(themeName)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		for _, todo := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/todo=%v", themeName, todo), func(t *testing.T) {
				m := newPlacementSidebar(t, false)
				m.SetSize(40, 60)
				base := "queue:item:"
				text := "Unicode 界é 👩‍💻\ncontinuation"
				if todo {
					base = "todo:item:"
					_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "s"}, Todos: []session.Todo{{ID: "item", Description: text, Status: "completed"}}})
					settlePlacement(t, m, cmd)
				} else {
					settlePlacement(t, m, m.SetQueuedMessages([]QueuedMessage{{ID: "item", Text: text}}))
				}
				first := requirePlaced(t, m, base+"0")
				next := requirePlaced(t, m, base+"1")
				width := m.contentWidth(m.cachedNeedsScrollbar)
				idle := m.placementText(first, width)
				for _, reveal := range []float64{.5, 1} {
					m.hoverValues = map[string]hoverValue{base + "row": {value: reveal, target: reveal}}
					baseline := sidebarCells(m.placementText(first, width))
					continuation := m.placementText(next, width)
					for _, progress := range []float64{0, .5, 1, .5, 0} {
						m.hoverValues[base+"remove"] = hoverValue{value: progress, target: progress}
						paint := m.placementText(first, width)
						cells := sidebarCells(paint)
						require.Len(t, cells, width)
						require.Equal(t, baseline[:width-1], cells[:width-1], "text, status and edit styling remain unchanged")
						require.Equal(t, continuation, m.placementText(next, width))
						require.Equal(t, "×", cells[width-1].glyph)
						require.Nil(t, cells[width-1].bg, "remove has no opaque background")
						r, g, b := styles.ColorToRGB(styles.Brighten(styles.MutedStyle.GetForeground(), .25*reveal))
						er, eg, eb := styles.ColorToRGB(styles.Error)
						emphasis := progress
						want := styles.RGBToColor(r+(er-r)*emphasis, g+(eg-g)*emphasis, b+(eb-b)*emphasis)
						require.Equal(t, color.NRGBAModel.Convert(want), color.NRGBAModel.Convert(cells[width-1].fg), "progress=%v reveal=%v", progress, reveal)
						if progress == 1 {
							require.Equal(t, color.NRGBAModel.Convert(styles.Error), color.NRGBAModel.Convert(cells[width-1].fg), "remove stays exactly theme error red, without extra brightening")
						}
						require.Equal(t, first, requirePlaced(t, m, first.id), "canonical text stays intact")
						require.Zero(t, m.ar.ActiveCount(), "painting does not schedule ticks")
					}
				}
				m.hoverValues = nil
				require.Equal(t, idle, m.placementText(first, width))
				require.NotContains(t, ansi.Strip(idle), "×")
			})
		}
	}
}
