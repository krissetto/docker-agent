package chat

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestTodoBodyClickRoutesDirectEditorAndClearsOnInterruption(t *testing.T) {
	for _, interruption := range []string{"none", "modal", "outside", "key", "wheel"} {
		t.Run(interruption, func(t *testing.T) {
			p := newLayoutTestPage(t, msgtypes.SidebarRight)
			t.Cleanup(func() { p.sidebar.(interface{ StopAnimation() }).StopAnimation() })
			p.SetSize(160, 40)
			scope := msgtypes.TodoScope{Owner: "owner", SessionID: "session", Generation: 2, Epoch: 3}
			p.Update(msgtypes.TodosSnapshotMsg{Scope: scope, Todos: []session.Todo{{ID: "opaque", Description: "Unique task body", Status: "pending"}}})
			p.sidebar.(interface{ CancelPresentation() }).CancelPresentation()
			p.sidebar.ReconcileLayout()
			x := styles.AppPadding + p.computeSidebarLayout().sidebarStartX + sidebar.DefaultLayoutConfig().PaddingLeft + 6
			y := renderedLineContaining(t, p.sidebar.View(), "todos")
			p.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			p.sidebar.(interface{ CancelPresentation() }).CancelPresentation()
			p.sidebar.ReconcileLayout()
			y = renderedLineContaining(t, p.sidebar.View(), "Unique task body")
			require.Equal(t, TargetSidebarContent, NewHitTest(p).At(x, y))
			click := tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
			findActions := func(cmd tea.Cmd) []tea.Msg {
				var actions []tea.Msg
				var visit func(tea.Cmd)
				visit = func(cmd tea.Cmd) {
					if cmd == nil {
						return
					}
					switch msg := cmd().(type) {
					case msgtypes.OpenTodoEditMsg, msgtypes.OpenTodosMsg:
						actions = append(actions, msg)
					case tea.BatchMsg:
						for _, child := range msg {
							visit(child)
						}
					}
				}
				visit(cmd)
				return actions
			}
			_, cmd := p.handleMouseClick(click)
			require.Empty(t, findActions(cmd), "single-click body emits no dialog action")
			switch interruption {
			case "modal":
				ClearSidebarHover(p)
			case "outside":
				p.handleMouseClick(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseRight})
			case "key":
				p.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			case "wheel":
				p.Update(msgtypes.WheelCoalescedMsg{X: 0, Y: 0, Delta: 1})
			}
			_, cmd = p.handleMouseClick(click)
			if interruption == "none" {
				require.Equal(t, []tea.Msg{msgtypes.OpenTodoEditMsg{Scope: scope, ID: "opaque"}}, findActions(cmd))
			} else {
				require.Empty(t, findActions(cmd), "interruption cancels the body pair")
			}
		})
	}
}
