package messages

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestWriteExpandedWorkFixture(t *testing.T) {
	dir := os.Getenv("DOCKER_AGENT_WORK_FIXTURE_DIR")
	if dir == "" {
		t.Skip("set DOCKER_AGENT_WORK_FIXTURE_DIR to export the screenshot-comparable Work fixture")
	}
	ar := animation.NewRuntime()
	m := NewScrollableView(ar, 104, 34, &service.SessionState{}).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.addMessage(types.User("Trace the deployment flow, verify the change, and report what remains."))
	m.AppendReasoning("root", "I am checking the deployment entry point and the test harness before editing.")
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "The first pass found a stale timeout in the deployment wrapper."))
	m.AppendReasoning("root", "I updated the stale timeout and am verifying it with focused tests.")
	completed := tools.ToolCall{ID: "fixture-check", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"go test ./pkg/deploy/..."}`}}
	m.AddOrUpdateToolCall("root", completed, tools.Tool{Name: "shell"}, types.ToolStatusRunning)
	m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: completed.ID, Response: "2 packages passed", Result: &tools.ToolCallResult{Output: "2 packages passed"}}, types.ToolStatusCompleted)
	m.AppendReasoning("root", "The focused check passes. I am checking one adjacent caller before giving the final summary.")
	m.SetWorkStatus(true, 0)
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", "Updated the deployment timeout, confirmed the focused tests, and kept the adjacent caller unchanged."))
	m.syncWorkProjection()
	block := firstWorkBlock(m)
	require.NotNil(t, block, "fixture projects tool and reasoning activity into Work")
	block.SetExpandedImmediately(true)
	for i, view := range m.views {
		if reasoning, ok := view.(*reasoningblock.Model); ok {
			reasoning.SetExpandedImmediately(true)
			reasoning.SetToolCallExpandedImmediately(completed.ID, true)
			m.invalidateItem(i)
		}
	}
	m.invalidateAllItems()
	m.ensureAllItemsRendered()
	plain := ansi.Strip(m.View())
	require.Contains(t, plain, "Working [-]")
	require.Contains(t, plain, "first pass")
	require.Contains(t, plain, "go test ./pkg/deploy")
	require.Contains(t, plain, "2 packages passed")
	require.Contains(t, plain, "Updated the deployment timeout")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "work-expanded-ansi.txt"), []byte(m.View()), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "work-expanded-plain.txt"), []byte(plain), 0o644))
}
