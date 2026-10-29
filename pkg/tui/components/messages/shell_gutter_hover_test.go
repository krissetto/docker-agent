package messages

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentmessage"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Transcript-column of the body scrollbar gutter for a standalone shell card.
func shellGutterColumn(contentWidth int) int {
	cardLeft := styles.UserMessageStyle.GetBorderLeftSize() + styles.UserMessageStyle.GetPaddingLeft()
	return cardLeft + agentmessage.InnerWidth(contentWidth) - 1
}

// gutterCell returns the rendered glyph in the scrollbar column of one transcript row.
func gutterCell(m *model, row, col int) string {
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	runes := []rune(lines[row])
	if col >= len(runes) {
		return ""
	}
	return string(runes[col])
}

func settleShellScrollbar(t *testing.T, scheduler *shellDisclosureScheduler, ar *animation.Runtime, view interface{ NeedsTick() bool }, m *model) {
	t.Helper()
	for range 20 {
		if !view.NeedsTick() {
			return
		}
		advanceShellDisclosure(t, ar, scheduler, m)
		_ = m.View()
	}
	require.False(t, view.NeedsTick(), "scrollbar animation settles")
}

func TestStandaloneShellGutterHoverSurvivesRenderAndTicks(t *testing.T) {
	scheduler := &shellDisclosureScheduler{}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	t.Cleanup(ar.Stop)
	m := NewScrollableView(ar, 100, 30, &service.SessionState{}).(*model)
	t.Cleanup(m.StopAnimations)
	call := shellCall("gutter", `{"cmd":"printf gutter"}`)
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: strings.Repeat("gutter row\n", 40)})
	view := m.views[0].(interface {
		SetExpanded(expanded bool)
		NeedsTick() bool
	})
	view.SetExpanded(true)
	m.invalidateItem(0)
	_ = m.View()

	bodyRow := m.lineOffsets[0] + 2
	gutterX := shellGutterColumn(m.contentWidth())
	require.Equal(t, " ", gutterCell(m, bodyRow, gutterX), "scrollbar starts hidden")

	m.handleMouseMotion(tea.MouseMotionMsg{X: gutterX, Y: bodyRow})
	require.True(t, view.NeedsTick(), "gutter hover starts the scrollbar reveal")
	settleShellScrollbar(t, scheduler, ar, view, m)
	require.NotEqual(t, " ", gutterCell(m, bodyRow, gutterX), "the pointer-held reveal survives render and tick settling")

	m.handleMouseMotion(tea.MouseMotionMsg{X: agentmessage.InnerWidth(m.contentWidth()) / 2, Y: bodyRow})
	settleShellScrollbar(t, scheduler, ar, view, m)
	require.Equal(t, " ", gutterCell(m, bodyRow, gutterX), "text-column hover lets the scrollbar fade out")
}
