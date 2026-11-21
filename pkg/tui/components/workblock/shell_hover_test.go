package workblock

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/tool/shell"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestShellHoverInvalidatesOnlyChangedWorkChildren(t *testing.T) {
	for _, positional := range []bool{false, true} {
		name := "line"
		if positional {
			name = "position"
		}
		t.Run(name, func(t *testing.T) {
			ar := animation.NewRuntime()
			t.Cleanup(ar.Stop)
			model := New(ar)
			t.Cleanup(model.StopAnimation)
			model.SetSize(100, 40)
			var items []Item
			for i, command := range []string{"printf first", "printf second"} {
				msg := types.ToolCallMessage("root", tools.ToolCall{Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"` + command + `"}`}}, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
				msg.Content = "readable detail output"
				child := shell.New(ar, msg, service.StaticSessionState{})
				t.Cleanup(func() { animation.StopView(child) })
				items = append(items, Item{SourceIndex: i, View: child})
			}
			untouched := &fakeView{text: "unrelated"}
			items = append(items, Item{SourceIndex: 2, View: untouched})
			model.SetItems(items)
			model.SetExpandedImmediately(true)
			baseline := model.View()
			muted := func(command string) string { return styles.RenderComposite(styles.ToolMessageStyle, command) }
			bright := func(command string) string { return styles.RenderComposite(styles.ToolDescription, command) }
			require.Contains(t, baseline, muted("printf first"))
			require.Contains(t, baseline, muted("printf second"))
			renders := untouched.views
			hover := func(source int, on bool) bool {
				t.Helper()
				line := -1
				for _, row := range model.Rows() {
					if row.SourceIndex == source {
						line = row.Start
					}
				}
				if positional {
					return model.SetHoveredPositionAt(line, BodyMargin(100)+5, on)
				}
				return model.SetHoveredAt(line, on)
			}
			require.True(t, hover(0, true))
			require.Contains(t, model.View(), bright("printf first"), "cached child repaints on hover entry")
			require.False(t, hover(0, true), "unchanged hover needs no repaint")
			child := items[0].View.(interface{ SetExpanded(bool) })
			child.SetExpanded(true)
			model.InvalidateSource(0)
			require.Contains(t, ansi.Strip(model.View()), "readable detail output")
			child.SetExpanded(false)
			model.InvalidateSource(0)
			require.Contains(t, model.View(), bright("printf first"), "collapse preserves genuine pointer hover")
			require.True(t, hover(1, true))
			require.Contains(t, model.View(), muted("printf first"), "moving to another child clears old hover")
			require.Contains(t, model.View(), bright("printf second"))
			require.True(t, hover(-1, true), "a blank Work row clears child hover")
			require.Equal(t, baseline, model.View())
			require.True(t, hover(0, true))
			_ = model.View()
			require.True(t, hover(0, false), "leaving Work repaints the cached hovered child")
			require.Equal(t, baseline, model.View())
			require.Equal(t, renders, untouched.views, "hover does not re-render unrelated children")
			require.Equal(t, strings.Count(baseline, "\n"), strings.Count(model.View(), "\n"))
		})
	}
}
