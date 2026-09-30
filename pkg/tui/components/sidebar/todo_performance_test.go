package sidebar

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func BenchmarkTodoSidebarLifecycle(b *testing.B) {
	for _, count := range []int{42, 100} {
		for _, collapsed := range []bool{false, true} {
			for _, mode := range []string{"idle", "motion", "updates", "scroll", "stream"} {
				b.Run(fmt.Sprintf("%d/collapsed=%v/%s", count, collapsed, mode), func(b *testing.B) {
					state := service.NewSessionState(session.New())
					state.SetCurrentAgentName("root")
					m := New(animation.NewRuntime(), b.Context(), state).(*model)
					defer m.StopAnimation()
					m.SetMode(ModeVertical)
					m.SetSize(40, 40)
					m.todosCollapsed = collapsed
					items := make([]session.Todo, count)
					for i := range items {
						items[i] = session.Todo{ID: fmt.Sprint(i), Status: []string{"pending", "in-progress", "completed"}[i%3], Description: strings.Repeat("Review 世界é deterministic long task description ", 3)}
					}
					_ = m.SetTodos(&tools.ToolCallResult{Meta: items})
					m.ReconcileLayout()
					m.View()
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						switch mode {
						case "motion":
							m.Update(tea.MouseMotionMsg{X: m.xPos + m.layoutCfg.PaddingLeft + 8, Y: m.yPos + m.todoSummaryLine + 2})
						case "updates":
							items[0].Status = []string{"completed", "pending"}[i%2]
							_ = m.SetTodos(&tools.ToolCallResult{Meta: items})
							m.ReconcileLayout()
						case "scroll":
							m.scrollview.ScrollBy(1)
						case "stream":
							m.invalidateAnimation()
							m.ReconcileLayout()
						}
						m.View()
					}
				})
			}
		}
	}
}
