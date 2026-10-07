package chat

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestOrderedChatAttachQueuesLateResultsWithoutRegrouping(t *testing.T) {
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{"value":"synthetic"}`}}
	def := tools.Tool{Name: "check"}
	first := &session.Message{AgentName: "worker", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ReasoningContent: "first", Presentation: []chatmsg.AssistantPart{{Type: chatmsg.AssistantPartReasoning, Text: "first"}, {Type: chatmsg.AssistantPartToolCall, ToolCallID: "call"}}, ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{def}}}
	history := session.New(session.WithID("s"))
	history.AddMessage(first)
	next := call
	next.ID = "next"
	second := &session.Message{AgentName: "worker", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ReasoningContent: "second", Presentation: []chatmsg.AssistantPart{{Type: chatmsg.AssistantPartReasoning, Text: "second"}, {Type: chatmsg.AssistantPartToolCall, ToolCallID: "next"}}, ToolCalls: []tools.ToolCall{next}, ToolDefinitions: []tools.Tool{def}}}
	firstResult := runtime.ToolCallResponse("call", def, &tools.ToolCallResult{Output: "late first result"}, "late first result", "worker")
	secondResult := runtime.ToolCallResponse("next", def, &tools.ToolCallResult{Output: "late second result"}, "late second result", "worker")
	newPage := func(sess *session.Session) *chatPage {
		a, _ := newSessionTestApp(t, sess, nil, nil)
		ar := animation.NewRuntime()
		state := service.NewSessionState(sess)
		p := New(ar, t.Context(), a, state).(*chatPage)
		p.messages.SetSize(100, 100)
		t.Cleanup(func() { p.messages.StopAnimations(); ar.Stop() })
		return p
	}
	live := newPage(session.New(session.WithID("s")))
	for _, event := range []runtime.Event{runtime.AgentChoiceReasoning("worker", "s", "first"), runtime.ToolCall(call, def, "worker"), runtime.MessageAddedAt("s", first, "worker", 0)} {
		live.handleRuntimeEvent(event)
	}
	attached := newPage(history)
	prepared := attached.beginReplay(runtime.SessionSnapshot{Session: history, TranscriptPosition: 1})()
	handled, _ := attached.updateReplay(prepared)
	require.True(t, handled)
	// A seed upgrades the snapshot call; response and subsequent round arrive while replay is installing.
	events := []runtime.Event{runtime.ToolCall(call, def, "worker"), firstResult, runtime.AgentChoiceReasoning("worker", "s", "second"), runtime.ToolCall(next, def, "worker"), runtime.MessageAddedAt("s", second, "worker", 2), secondResult}
	for _, event := range events {
		handled, _ = attached.updateReplay(msgtypes.SessionRuntimeEventMsg{Event: event})
		require.True(t, handled)
		live.handleRuntimeEvent(event)
	}
	require.Len(t, attached.replay.queued, len(events))
	for range 100 {
		if attached.replay == nil {
			break
		}
		attached.updateReplay(replayContinueMsg{generation: attached.replay.generation})
	}
	require.Nil(t, attached.replay)
	history.AddMessage(&session.Message{AgentName: "worker", Message: chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: "call", Content: "late first result"}})
	history.AddMessage(second)
	history.AddMessage(&session.Message{AgentName: "worker", Message: chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: "next", Content: "late second result"}})
	replay := newPage(history)
	replay.messages.LoadFromSession(history, nil)
	for _, path := range []struct {
		name string
		p    *chatPage
	}{{"live", live}, {"async_attach_queued_results", attached}, {"history", replay}} {
		require.Equal(t, 2, path.p.messages.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
		require.Zero(t, path.p.messages.MessageTypeCount(types.MessageTypeToolCall))
		frame := ansi.Strip(path.p.messages.View())
		require.Equal(t, 2, strings.Count(frame, "Thinking [+] (1 tool)"))
		t.Logf("%s: two adjacent collapsed reasoning blocks, no standalone tools", path.name)
	}
	// Tool results are usually folded in collapsed reasoning, so validate by expanding every block.
	for _, path := range []struct {
		name string
		p    *chatPage
	}{{"live", live}, {"async_attach_queued_results", attached}, {"history", replay}} {
		path.p.messages.Focus()
		path.p.messages.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		path.p.messages.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		path.p.messages.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		frame := ansi.Strip(path.p.messages.View())
		require.True(t, strings.Contains(frame, "late first result"), path.name)
		require.True(t, strings.Contains(frame, "late second result"), path.name)
	}
}

func TestOrderedChatMidResponseAttachPreservesSeedAndQueuedOrder(t *testing.T) {
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{"value":"synthetic"}`}}
	def := tools.Tool{Name: "check"}
	snapshotSession := session.New(session.WithID("s"))
	snapshotSession.AddMessage(session.UserMessage("synthetic task"))
	newPage := func(sess *session.Session) *chatPage {
		a, _ := newSessionTestApp(t, sess, nil, nil)
		ar := animation.NewRuntime()
		state := service.NewSessionState(sess)
		state.SetExpandThinking(true)
		p := New(ar, t.Context(), a, state).(*chatPage)
		p.messages.SetSize(100, 100)
		t.Cleanup(func() { p.messages.StopAnimations(); ar.Stop() })
		return p
	}
	seed := []runtime.Event{
		runtime.AgentChoiceReasoning("worker", "s", "first thought"),
		runtime.ToolCall(call, def, "worker"),
		runtime.AgentChoice("worker", "s", "before commentary"),
	}
	committed := &session.Message{AgentName: "worker", Message: chatmsg.Message{
		Role: chatmsg.MessageRoleAssistant, ReasoningContent: "first thoughtsecond thought", Content: "before commentaryafter commentary",
		ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{def},
		Presentation: []chatmsg.AssistantPart{
			{Type: chatmsg.AssistantPartReasoning, Text: "first thought"},
			{Type: chatmsg.AssistantPartToolCall, ToolCallID: call.ID},
			{Type: chatmsg.AssistantPartContent, Text: "before commentary"},
			{Type: chatmsg.AssistantPartReasoning, Text: "second thought"},
			{Type: chatmsg.AssistantPartContent, Text: "after commentary"},
		},
	}}
	live := newPage(snapshotSession)
	live.messages.LoadFromSession(snapshotSession, nil)
	for _, event := range seed {
		live.handleRuntimeEvent(event)
	}
	attached := newPage(snapshotSession)
	prepared := attached.beginReplay(runtime.SessionSnapshot{
		Session: snapshotSession, TranscriptPosition: 1,
		Status:       runtime.SessionStatus{SessionID: "s", AgentName: "worker", State: runtime.SessionStateRunning},
		Presentation: seed,
	})()
	handled, _ := attached.updateReplay(prepared)
	require.True(t, handled)
	remaining := []runtime.Event{
		runtime.AgentChoiceReasoning("worker", "s", "second thought"),
		runtime.AgentChoice("worker", "s", "after commentary"),
		runtime.MessageAddedAt("s", committed, "worker", 1),
		runtime.ToolCallResponse("call", def, &tools.ToolCallResult{Output: "late result"}, "late result", "worker"),
	}
	for _, event := range remaining {
		handled, _ = attached.updateReplay(msgtypes.SessionRuntimeEventMsg{Event: event})
		require.True(t, handled)
		live.handleRuntimeEvent(event)
	}
	require.Len(t, attached.replay.queued, len(remaining), "events arrive while transcript installation is pending")
	for range 100 {
		if attached.replay == nil {
			break
		}
		attached.updateReplay(replayContinueMsg{generation: attached.replay.generation})
	}
	require.Nil(t, attached.replay)
	history := snapshotSession.Clone()
	history.AddMessage(committed)
	history.AddMessage(&session.Message{AgentName: "worker", Message: chatmsg.Message{Role: chatmsg.MessageRoleTool, ToolCallID: call.ID, Content: "late result"}})
	data, err := json.Marshal(history)
	require.NoError(t, err)
	var persisted session.Session
	require.NoError(t, json.Unmarshal(data, &persisted))
	replay := newPage(&persisted)
	replay.messages.LoadFromSession(&persisted, nil)
	var expectedFrame string
	for _, path := range []struct {
		name string
		page *chatPage
	}{{"live", live}, {"mid_response_attach", attached}, {"JSON_replay", replay}} {
		require.Equal(t, 2, path.page.messages.MessageTypeCount(types.MessageTypeAssistantReasoningBlock), path.name)
		require.Equal(t, 2, path.page.messages.MessageTypeCount(types.MessageTypeAssistant), path.name)
		require.Zero(t, path.page.messages.MessageTypeCount(types.MessageTypeToolCall), path.name)
		frame := ansi.Strip(path.page.messages.View())
		require.Equal(t, 2, strings.Count(frame, "Thinking [-]"), path.name)
		previous := -1
		for _, text := range []string{"synthetic task", "first thought", "late result", "before commentary", "second thought", "after commentary"} {
			position := strings.Index(frame, text)
			require.Greater(t, position, previous, "%s order of %s", path.name, text)
			require.Equal(t, 1, strings.Count(frame, text), "%s duplicates %s", path.name, text)
			previous = position
		}
		if expectedFrame == "" {
			expectedFrame = frame
		} else {
			require.Equal(t, expectedFrame, frame, path.name)
		}
	}
}
