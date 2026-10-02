package chat

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	messagecomponent "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/sidebar"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestReplayStreamStartedShowsAttachedPending(t *testing.T) {
	t.Parallel()

	state := &service.SessionState{}
	page := &chatPage{messages: messagecomponent.New(animation.NewRuntime(), state), sidebar: sidebar.New(animation.NewRuntime(), t.Context(), state)}
	before := page.messages.View()

	// Supervisor maps a reconnect replay's nonzero sequence to Seed=false.
	page.handleRuntimeEvent(msgtypes.SessionRuntimeEventMsg{Event: runtime.StreamStarted("replay-child", "worker"), Seed: false})
	assert.NotEqual(t, before, page.messages.View(), "replayed genuine start must show attached pending")
}

func TestOrdinaryStartShowsPendingEvenWhenSeedMarked(t *testing.T) {
	t.Parallel()

	state := &service.SessionState{}
	page := &chatPage{messages: messagecomponent.New(animation.NewRuntime(), state), sidebar: sidebar.New(animation.NewRuntime(), t.Context(), state)}
	before := page.messages.View()
	page.handleRuntimeEvent(msgtypes.SessionRuntimeEventMsg{Event: runtime.StreamStarted("root", "root"), Seed: true})
	assert.NotEqual(t, before, page.messages.View())
}

func TestToolActivityClearsFirstResponsePending(t *testing.T) {
	for _, result := range []bool{false, true} {
		state := &service.SessionState{}
		page := &chatPage{messages: messagecomponent.New(animation.NewRuntime(), state), sidebar: sidebar.New(animation.NewRuntime(), t.Context(), state)}
		page.handleToolCall(&runtime.ToolCallEvent{ToolCall: tools.ToolCall{ID: "call"}})
		page.handleStreamStarted(runtime.StreamStarted("s", "worker").(*runtime.StreamStartedEvent), false)
		withSpinner := page.messages.View()
		if result {
			page.handleToolCallResponse(&runtime.ToolCallResponseEvent{ToolCallID: "call", Result: &tools.ToolCallResult{Output: "done"}})
		} else {
			page.handleToolCallOutput(&runtime.ToolCallOutputEvent{ToolCallID: "call", Output: "output"})
		}
		assert.NotEqual(t, withSpinner, page.messages.View())
		withoutSpinner := page.messages.View()
		page.setPendingResponse(false)
		assert.Equal(t, withoutSpinner, page.messages.View())
	}
}
