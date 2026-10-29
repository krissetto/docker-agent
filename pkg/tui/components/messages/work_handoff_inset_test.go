package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	builtinshell "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Expanded Work children keep their two-column inset in every reply-handoff frame.
func TestExpandedWorkShellKeepsInsetThroughHandoffFrames(t *testing.T) {
	scheduler := &replyHandoffScheduler{now: testHandoffTime()}
	ar := animation.NewRuntimeWithScheduler(scheduler)
	m := NewScrollableView(ar, 84, 50, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetPosition(0, 0)
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("request"))
	m.AppendReasoning("root", "planning the shell step")
	call := tools.ToolCall{ID: "handoff-shell", Function: tools.FunctionCall{Name: builtinshell.ToolNameShell, Arguments: `{"cmd":"printf probe"}`}}
	m.AddOrUpdateToolCall("root", call, tools.Tool{Name: builtinshell.ToolNameShell}, types.ToolStatusCompleted)
	m.AppendToolOutput(&runtime.ToolCallOutputEvent{ToolCallID: call.ID, Output: "probe output line\nsecond"})
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "retained answer"))
	m.workProjectionDirty = true
	m.syncWorkProjection()
	if work := firstWorkBlock(m); work != nil {
		work.SetExpandedImmediately(true)
	}
	for _, v := range m.views {
		if block, ok := v.(*reasoningblock.Model); ok {
			block.SetExpandedImmediately(true)
			block.SetToolCallExpandedImmediately(call.ID, true)
		}
	}
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	_ = m.View()

	toolExpanded := true
	assertCardInset := func(stage string) {
		t.Helper()
		found := false
		if !toolExpanded {
			return
		}
		for line := range strings.SplitSeq(ansi.Strip(m.View()), "\n") {
			if strings.Contains(line, "probe output line") {
				found = true
				require.Equal(t, 2, len(line)-len(strings.TrimLeft(line, " ")), "%s: nested card body keeps the Work inset: %q", stage, line)
			}
		}
		require.True(t, found, "%s: nested shell output is visible", stage)
	}
	assertCardInset("settled")

	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "newly streaming prose"))
	require.True(t, m.replyHandoff.Active(), "the incoming reply starts a handoff that reflows the expanded Work")
	for i := 0; i < 200 && m.replyHandoff.Active(); i++ {
		switch i {
		case 2:
			m.SetSize(m.width-6, m.height)
		case 4:
			toolExpanded = false
			for _, v := range m.views {
				if block, ok := v.(*reasoningblock.Model); ok {
					block.SetToolCallExpandedImmediately(call.ID, false)
				}
			}
		case 6:
			toolExpanded = true
			for _, v := range m.views {
				if block, ok := v.(*reasoningblock.Model); ok {
					block.SetToolCallExpandedImmediately(call.ID, true)
				}
			}
		}
		acceptAndUpdateHandoffTick(t, ar, m)
		m.applyReplyHandoffPaint()
		assertCardInset(fmt.Sprintf("handoff tick %d", i))
	}
	assertCardInset("after handoff")
}
