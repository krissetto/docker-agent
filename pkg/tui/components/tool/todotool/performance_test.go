package todotool

import (
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func BenchmarkTodoLayout(b *testing.B) {
	for _, count := range []int{42, 100, 500} {
		for _, mode := range []string{"collapsed", "idle", "motion", "updates", "scroll", "stream"} {
			b.Run(fmt.Sprintf("%d/%s", count, mode), func(b *testing.B) {
				c := NewSidebarComponent()
				c.SetSize(32)
				items := make([]todo.Todo, count)
				for i := range items {
					items[i] = todo.Todo{ID: fmt.Sprint(i), Status: []string{"pending", "in-progress", "completed"}[i%3], Description: strings.Repeat("Review 世界é deterministic long task description ", 3)}
				}
				_ = c.SetTodos(&tools.ToolCallResult{Meta: items})
				body := c.RenderBody()
				lines := strings.Count(body, "\n") + 1
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					switch mode {
					case "collapsed":
						c.Counts()
					case "idle", "stream":
						c.RenderBody()
					case "motion", "scroll":
						for line := 0; line < min(lines, 30); line++ {
							c.TodoAtLine(line)
							c.ControlsAtLine(line)
						}
					case "updates":
						items[0].Status = []string{"completed", "pending"}[n%2]
						_ = c.SetTodos(&tools.ToolCallResult{Meta: items})
						c.RenderBody()
						for line := 0; line < lines; line++ {
							c.TodoAtLine(line)
							c.ControlsAtLine(line)
						}
					}
				}
			})
		}
	}
}
