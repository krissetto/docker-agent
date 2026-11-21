package messages

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestShellHoverBrightnessAfterAnimatedToggle(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("compact=%t/selected=%t", compact, selected), func(t *testing.T) {
				ar := animation.NewRuntimeWithScheduler(&disclosureClock{now: time.Unix(1, 0)})
				m := NewScrollableView(ar, 100, 30, &service.SessionState{}).(*model)
				t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
				m.SetCompactWorkLog(compact)
				m.suppressReplyHandoff = true
				m.addMessage(types.User("request"))
				for i, command := range []string{"printf first", "printf second"} {
					call := shellCall(fmt.Sprint(i), `{"cmd":"`+command+`"}`)
					m.AddOrUpdateToolCall("root", call, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
					m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: "readable shell output"})
				}
				m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "final answer"))
				_ = m.View()
				if compact {
					block := firstWorkBlock(m)
					require.NotNil(t, block)
					block.SetExpandedImmediately(true)
				}
				if selected {
					m.Focus()
					m.selectedMessageIndex = 1
				}
				m.invalidateAllItems()
				_ = m.View()
				r := &hoverRepro{t: t, ar: ar, m: m}
				r.settle()
				preview := func(command string, hovered bool) string {
					style := styles.ToolMessageStyle
					if hovered {
						style = styles.ToolDescription
					}
					return styles.RenderComposite(style, command)
				}
				assertPreview := func(command string, hovered bool) {
					t.Helper()
					require.Contains(t, m.View(), preview(command, hovered))
				}
				move := func(command string) {
					t.Helper()
					rows := strings.Split(m.View(), "\n")
					y := rowOf(rows, command)
					require.GreaterOrEqual(t, y, 0)
					x := strings.Index(ansi.Strip(rows[y]), "shell")
					require.GreaterOrEqual(t, x, 0)
					m.handleMouseMotion(tea.MouseMotionMsg{X: x, Y: y})
					_ = m.View()
				}
				assertPreview("printf first", false)
				move("printf first")
				assertPreview("printf first", true)
				for range 2 {
					rows := strings.Split(m.View(), "\n")
					y := rowOf(rows, "shell")
					x := strings.Index(ansi.Strip(rows[y]), "shell")
					m.handleMouseClick(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
					_ = m.View()
					r.settle()
					if shellIsExpanded(m, 1) {
						require.Contains(t, ansi.Strip(m.View()), "readable shell output")
					}
				}
				require.False(t, shellIsExpanded(m, 1))
				assertPreview("printf first", true)
				move("printf second")
				assertPreview("printf first", false)
				assertPreview("printf second", true)
				m.handleMouseMotion(tea.MouseMotionMsg{X: 1000, Y: 1000})
				_ = m.View()
				assertPreview("printf first", false)
				assertPreview("printf second", false)
			})
		}
	}
}
