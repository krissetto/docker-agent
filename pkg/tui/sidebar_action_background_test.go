package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestSidebarActionRevealAcrossCanvasTransparency(t *testing.T) {
	setupAutoThemeTest(t)
	restore := sidebar.SetWorkingDirectoryForTesting("/workspace/project", "")
	t.Cleanup(restore)
	for _, ref := range []string{"default", "default-light", "gruvbox-dark"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		ar := animation.NewRuntimeWithScheduler(&rootImmediateScheduler{now: time.Unix(1, 0)})
		pane := sidebar.New(ar, t.Context(), &service.SessionState{})
		pane.SetSize(40, 60)
		pane.SetQueuedMessages([]sidebar.QueuedMessage{{ID: "queued", Text: "queued first\nqueued continuation"}})
		require.NoError(t, pane.SetTodos(&tools.ToolCallResult{Meta: []session.Todo{{ID: "todo", Description: "todo first\ntodo continuation", Status: "completed"}}}))
		pane.ReconcileLayout()
		pane.SetPresentationActive(false)
		pane.SetPresentationActive(true)
		for y, line := range strings.Split(ansi.Strip(pane.View()), "\n") {
			if strings.Contains(line, "1/1 todos") {
				pane.Update(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
				break
			}
		}
		pane.SetPresentationActive(false)
		pane.SetPresentationActive(true)
		settle := func() {
			for range 40 {
				cmd := ar.Continue()
				if cmd == nil {
					return
				}
				tick, accepted := ar.Accept(cmd().(animation.TickMsg))
				require.True(t, accepted)
				pane.Update(tick)
			}
			t.Fatal("hover lease failed to settle")
		}
		for _, label := range []string{"project", "queued continuation", "todo continuation"} {
			lines := strings.Split(ansi.Strip(pane.View()), "\n")
			y := -1
			for i, line := range lines {
				if strings.Contains(line, label) {
					y = i
					break
				}
			}
			require.GreaterOrEqual(t, y, 0, "%s", label)
			pane.Update(tea.MouseMotionMsg{X: 4, Y: y})
			settle()
			for _, transparent := range []bool{true, false} {
				root := &appModel{transparentBackground: transparent}
				active := root.canvasView(pane.View(), "", false, false).Content
				if label == "project" {
					require.Contains(t, ansi.Strip(active), "⎘  ↗")
				} else {
					require.Contains(t, ansi.Strip(active), "✎ ×")
				}
				for _, line := range strings.Split(ansi.Strip(active), "\n") {
					if strings.Contains(line, "continuation") {
						require.NotContains(t, line, "✎")
						require.NotContains(t, line, "×")
					}
				}
			}
			pane.ClearSubagentHover()
			settle()
			for _, transparent := range []bool{true, false} {
				root := &appModel{transparentBackground: transparent}
				inactive := root.canvasView(pane.View(), "", false, false).Content
				for _, glyph := range []string{"⎘", "↗", "✎", "×"} {
					require.NotContains(t, ansi.Strip(inactive), glyph)
				}
				require.Zero(t, ar.ActiveCount())
			}
		}
		pane.SetPresentationActive(false)
	}
}
