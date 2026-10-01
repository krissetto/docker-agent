package sidebar

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestItemHoverRevealsOnlyFirstVisualLine(t *testing.T) {
	for _, width := range []int{9, 12, 20, 40, 80} {
		for _, todo := range []bool{false, true} {
			t.Run(fmt.Sprintf("width=%d/todo=%v", width, todo), func(t *testing.T) {
				m := newPlacementSidebar(t, false)
				m.SetSize(width, 100)
				text := "界é 👩‍💻 first words\ncontinuation words"
				base := "queue:opaque:"
				if todo {
					base = "todo:opaque:"
					_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "s"}, Todos: []session.Todo{{ID: "opaque", Description: text, Status: "completed"}}})
					settlePlacement(t, m, cmd)
				} else {
					settlePlacement(t, m, m.SetQueuedMessages([]QueuedMessage{{ID: "opaque", Text: text}}))
				}
				first := requirePlaced(t, m, base+"0")
				next := requirePlaced(t, m, base+"1")
				w := m.contentWidth(m.cachedNeedsScrollbar)
				indent, actions := rowActions(w, todo)
				cluster := func(row placedRow) string {
					return ansi.Strip(ansi.Cut(m.placementText(row, w), w-4, w))
				}
				idle := m.placementText(first, w)
				require.NotContains(t, ansi.Strip(idle), "✎")
				require.NotContains(t, ansi.Strip(idle), "×")
				_, renders := m.CacheStats()
				cmd := m.updateRegionHover(m.layoutCfg.PaddingLeft+indent, int(next.y))
				cmd = advancePlacement(t, m, cmd)
				progress := animation.HoverStep(0, 1, 50*time.Millisecond)
				require.Equal(t, progress, m.hoverValues[base+"row"].value)
				if actions.Remove >= 0 {
					require.Contains(t, cluster(first), "✎ ×", "continuation hover reveals first-line actions")
					col := indent + actions.Edit
					glyph := styles.MutedStyle.Render("✎")
					require.Equal(t, sidebarCells(styles.HoverText(glyph, progress, styles.TextPrimary))[0], sidebarCells(m.placementText(first, w))[col])
				}
				require.NotContains(t, cluster(next), "✎")
				require.NotContains(t, cluster(next), "×")
				require.LessOrEqual(t, ansi.StringWidth(m.placementText(first, w)), w)
				settlePlacement(t, m, cmd)
				require.Zero(t, m.ar.ActiveCount())
				_, after := m.CacheStats()
				require.Equal(t, renders, after, "hover never rebuilds semantic sections")
				if todo {
					require.Contains(t, m.placementText(first, w), "\x1b[9m", "completed text retains strikethrough")
				}
				cmd = m.ClearSubagentHover()
				cmd = advancePlacement(t, m, cmd)
				require.Greater(t, m.hoverValues[base+"row"].value, 0.0)
				settlePlacement(t, m, cmd)
				require.Equal(t, idle, m.placementText(first, w))
				require.Zero(t, m.ar.ActiveCount())
			})
		}
	}
}

func TestActionHoverClearsOnScrollResizeAndModal(t *testing.T) {
	for _, todo := range []bool{false, true} {
		for _, clear := range []string{"scroll", "resize", "modal", "other-item"} {
			t.Run(fmt.Sprintf("todo=%v/%s", todo, clear), func(t *testing.T) {
				m := newPlacementSidebar(t, false)
				m.SetSize(40, 8)
				base := "queue:opaque:"
				if todo {
					base = "todo:opaque:"
					_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "s"}, Todos: []session.Todo{{ID: "opaque", Description: strings.Repeat("世界 long task ", 30), Status: "pending"}}})
					settlePlacement(t, m, cmd)
				} else {
					settlePlacement(t, m, m.SetQueuedMessages([]QueuedMessage{{ID: "opaque", Text: "first\ncontinuation"}, {ID: "second", Text: "second\ncontinued"}, {ID: "third", Text: "third\ncontinued"}, {ID: "fourth", Text: "fourth\ncontinued"}}))
				}
				first := requirePlaced(t, m, base+"0")
				m.scrollview.SetScrollOffset(int(first.y))
				settlePlacement(t, m, m.updateRegionHover(m.layoutCfg.PaddingLeft+2, int(first.y)-m.scrollview.ScrollOffset()))
				require.Equal(t, 1.0, m.hoverValues[base+"row"].value)
				if todo {
					m.todoRemoveArmed = "opaque"
				} else {
					require.False(t, m.ConfirmQueuedRemoval("opaque"))
				}
				switch clear {
				case "scroll":
					_, cmd := m.Update(messages.WheelCoalescedMsg{Delta: 1})
					settlePlacement(t, m, cmd)
				case "resize":
					settlePlacement(t, m, m.SetSize(41, 8))
				case "modal":
					m.CancelHover()
					m.ReconcileLayout()
				case "other-item":
					settlePlacement(t, m, m.setHoverTarget("directory"))
				}
				require.Zero(t, m.hoverValues[base+"row"].value)
				require.Empty(t, m.todoRemoveArmed)
				require.Empty(t, m.queueRemoveArmed)
				require.Zero(t, m.ar.ActiveCount())
				if clear == "scroll" {
					next := requirePlaced(t, m, base+"1")
					m.scrollview.SetScrollOffset(int(next.y))
					w := m.contentWidth(m.cachedNeedsScrollbar)
					settlePlacement(t, m, m.updateRegionHover(m.layoutCfg.PaddingLeft+w-1, 0))
					require.NotContains(t, ansi.Strip(strings.Split(m.View(), "\n")[0]), "✎")
					if todo {
						cmd, handled := m.todoClick(m.layoutCfg.PaddingLeft+w-1, 0)
						require.True(t, handled)
						require.Nil(t, cmd, "single-click todo continuation is inert")
					} else {
						result, id := m.HandleClickType(m.layoutCfg.PaddingLeft+w-1, 0)
						require.Equal(t, ClickQueuedMessage, result)
						require.Equal(t, "opaque", id)
					}
				}
			})
		}
	}
}

func TestIdleActionPaintFallbackAndThemeRefresh(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := newPlacementSidebar(t, false)
	m.SetSize(60, 80)
	settlePlacement(t, m, m.SetQueuedMessages([]QueuedMessage{{ID: "q", Text: "queued"}}))
	_, cmd := m.Update(messages.TodosSnapshotMsg{Scope: messages.TodoScope{SessionID: "s"}, Todos: []session.Todo{{ID: "t", Description: "completed task", Status: "completed"}}})
	settlePlacement(t, m, cmd)
	for _, placement := range []bool{true, false} {
		if !placement {
			m.placement = nil
		}
		theme, err := styles.LoadTheme("default-light")
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		view := ansi.Strip(m.View())
		for _, glyph := range []string{"✎", "×", directoryIcon, directoryCopyIcon} {
			require.NotContains(t, view, glyph)
		}
		require.Contains(t, view, "completed task")
	}
}
