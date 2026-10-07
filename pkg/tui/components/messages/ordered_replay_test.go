package messages

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/reasoningblock"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func orderedTestModel(t *testing.T) *model {
	t.Helper()
	ar := animation.NewRuntime()
	state := &service.SessionState{}
	state.SetExpandThinking(true)
	m := NewScrollableView(ar, 100, 100, state).(*model)
	t.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	return m
}

func orderedTopology(m *model) string {
	var parts []string
	for i, msg := range m.messages {
		switch msg.Type {
		case types.MessageTypeAssistantReasoningBlock:
			b := m.views[i].(*reasoningblock.Model)
			var ids []string
			for _, id := range []string{"call", "next"} {
				if b.HasToolCall(id) {
					ids = append(ids, id)
				}
			}
			parts = append(parts, fmt.Sprintf("R(%s;%s)", b.Reasoning(), strings.Join(ids, ",")))
		case types.MessageTypeAssistant:
			parts = append(parts, "C("+msg.Content+")")
		case types.MessageTypeToolCall:
			parts = append(parts, "T("+msg.ToolCall.ID+")")
		case types.MessageTypeUser:
			parts = append(parts, "U("+msg.Content+")")
		}
	}
	return strings.Join(parts, " ")
}

func TestOrderedAssistantLiveAttachAndPersistedReplay(t *testing.T) {
	for _, tc := range []struct{ name, order, want string }{
		{"reasoning_then_tool", "RT", "R(first;call)"},
		{"commentary_before_tool", "RCT", "R(first;) C(commentary) T(call)"},
		{"tool_before_commentary", "RTC", "R(first;call) C(commentary)"},
		{"tool_before_reasoning", "TR", "T(call) R(first;)"},
		{"interleaved_text", "RCRC", "R(first;) C(commentary) R(second;) C(after)"},
		{"interleaved_reasoning_commentary", "RCRT", "R(first;) C(commentary) R(second;call)"},
		{"reasoning_after_tool", "RTR", "R(first\n\nsecond;call)"},
		{"standalone_tool", "T", "T(call)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{"value":"synthetic"}`}}
			def := tools.Tool{Name: "check"}
			sess := session.New(session.WithID("s"))
			msg := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant}}
			live, attached := orderedTestModel(t), orderedTestModel(t)
			reasoningChunks, contentChunks := 0, 0
			for _, c := range tc.order {
				var part chat.AssistantPart
				switch c {
				case 'R':
					text := "first"
					if reasoningChunks > 0 {
						text = "second"
					}
					reasoningChunks++
					part = chat.AssistantPart{Type: chat.AssistantPartReasoning, Text: text}
					msg.Message.ReasoningContent += text
					live.AppendReasoning("worker", text)
				case 'C':
					text := "commentary"
					if contentChunks > 0 {
						text = "after"
					}
					contentChunks++
					part = chat.AssistantPart{Type: chat.AssistantPartContent, Text: text}
					msg.Message.Content += text
					live.AppendToLastMessage("worker", text)
				case 'T':
					part = chat.AssistantPart{Type: chat.AssistantPartToolCall, ToolCallID: call.ID}
					msg.Message.ToolCalls = []tools.ToolCall{call}
					msg.Message.ToolDefinitions = []tools.Tool{def}
					live.AddOrUpdateToolCall("worker", call, def, types.ToolStatusRunning)
				}
				msg.Message.Presentation = chat.AppendAssistantPart(msg.Message.Presentation, part)
			}
			require.Equal(t, tc.want, orderedTopology(live), "before commit")
			live.CompleteAssistant(runtime.MessageAddedAt("s", msg, "worker", 0).(*runtime.MessageAddedEvent))
			sess.AddMessage(msg)
			attached.LoadFromSession(sess, nil)
			result := &runtime.ToolCallResponseEvent{ToolCallID: call.ID, Response: "synthetic result", Result: &tools.ToolCallResult{Output: "synthetic result"}}
			if len(msg.Message.ToolCalls) > 0 {
				// A late seed must update the original location, not create a new bubble.
				attached.AddOrUpdateToolCall("worker", call, def, types.ToolStatusRunning)
				live.AddToolResult(result, types.ToolStatusCompleted)
				attached.AddToolResult(result, types.ToolStatusCompleted)
				sess.AddMessage(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: call.ID, Content: result.Response}})
			}
			data, err := json.Marshal(sess)
			require.NoError(t, err)
			var persisted session.Session
			require.NoError(t, json.Unmarshal(data, &persisted))
			replay := orderedTestModel(t)
			BeginReplay(replay, PrepareReplay(&persisted), nil)
			finishPreparedReplay(t, replay)
			for _, path := range []struct {
				name string
				m    *model
			}{{"live", live}, {"attach", attached}, {"persisted", replay}} {
				require.Equal(t, tc.want, orderedTopology(path.m), path.name)
				frame := ansi.Strip(path.m.View())
				if len(msg.Message.ToolCalls) > 0 {
					require.Contains(t, frame, "synthetic result", path.name)
				}
				if tc.order == "RTR" {
					require.Less(t, strings.Index(frame, "first"), strings.Index(frame, "synthetic result"), path.name)
					require.Less(t, strings.Index(frame, "synthetic result"), strings.Index(frame, "second"), path.name)
				}
			}
		})
	}
}

func TestOrderedAssistantResponseAndUserBoundaries(t *testing.T) {
	for _, userBoundary := range []bool{false, true} {
		for _, toolOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("user=%v/toolOnly=%v", userBoundary, toolOnly), func(t *testing.T) {
				sess := session.New(session.WithID("s"))
				first := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "first", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "first"}}}}
				call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{"value":"synthetic"}`}}
				first.Message.ToolCalls = []tools.ToolCall{call}
				first.Message.ToolDefinitions = []tools.Tool{{Name: "check"}}
				first.Message.Presentation = chat.AppendAssistantPart(first.Message.Presentation, chat.AssistantPart{Type: chat.AssistantPartToolCall, ToolCallID: call.ID})
				live := orderedTestModel(t)
				live.AppendReasoning("worker", "first")
				live.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "check"}, types.ToolStatusRunning)
				live.CompleteAssistant(runtime.MessageAddedAt("s", first, "worker", 0).(*runtime.MessageAddedEvent))
				sess.AddMessage(first)
				attached := orderedTestModel(t)
				attached.LoadFromSession(sess, nil)
				want := "R(first;call)"
				if userBoundary {
					input := session.UserMessage("next task")
					sess.AddMessage(input)
					live.AddInputMessage(input, 1)
					attached.AddInputMessage(input, 1)
					want += " U(next task)"
				}
				next := call
				next.ID = "next"
				second := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{next}, ToolDefinitions: []tools.Tool{{Name: "check"}}}}
				if !toolOnly {
					second.Message.ReasoningContent = "second"
					second.Message.Presentation = []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "second"}}
					live.AppendReasoning("worker", "second")
					attached.AppendReasoning("worker", "second")
					want += " R(second;next)"
				} else {
					want += " T(next)"
				}
				second.Message.Presentation = chat.AppendAssistantPart(second.Message.Presentation, chat.AssistantPart{Type: chat.AssistantPartToolCall, ToolCallID: next.ID})
				for _, m := range []*model{live, attached} {
					m.AddOrUpdateToolCall("worker", next, tools.Tool{Name: "check"}, types.ToolStatusRunning)
					m.CompleteAssistant(runtime.MessageAddedAt("s", second, "worker", sess.ItemCount()).(*runtime.MessageAddedEvent))
					m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "check"}, types.ToolStatusRunning)
					m.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "call", Response: "late first", Result: &tools.ToolCallResult{Output: "late first"}}, types.ToolStatusCompleted)
				}
				sess.AddMessage(second)
				sess.AddMessage(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call", Content: "late first"}})
				replay := orderedTestModel(t)
				replay.LoadFromSession(sess, nil)
				for _, m := range []*model{live, attached, replay} {
					require.Equal(t, want, orderedTopology(m))
					require.True(t, m.views[0].(*reasoningblock.Model).HasToolCall("call"))
					require.False(t, m.views[0].(*reasoningblock.Model).HasToolCall("next"))
					require.Contains(t, ansi.Strip(m.views[0].View()), "late first")
				}
			})
		}
	}
}

func TestOrderedAssistantLegacyAggregateFallback(t *testing.T) {
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{"value":"synthetic"}`}}
	m := orderedTestModel(t)
	sess := session.New(session.WithID("legacy"))
	sess.AddMessage(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "firstsecond", Content: "commentary", ToolCalls: []tools.ToolCall{call}}})
	m.LoadFromSession(sess, nil)
	require.Equal(t, "R(firstsecond;) C(commentary) T(call)", orderedTopology(m))
}

func TestOrderedAssistantDisplayOnlyHarnessTool(t *testing.T) {
	for _, isError := range []bool{false, true} {
		for _, order := range []string{"RTC", "TR"} {
			t.Run(fmt.Sprintf("error=%v/%s", isError, order), func(t *testing.T) {
				call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{"value":"synthetic"}`}}
				def := tools.Tool{Name: "check"}
				result := "harness result"
				msg := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant}}
				live := orderedTestModel(t)
				for _, c := range order {
					switch c {
					case 'R':
						live.AppendReasoning("worker", "first")
						msg.Message.Presentation = chat.AppendAssistantPart(msg.Message.Presentation, chat.AssistantPart{Type: chat.AssistantPartReasoning, Text: "first"})
						msg.Message.ReasoningContent = "first"
					case 'C':
						live.AppendToLastMessage("worker", "commentary")
						msg.Message.Presentation = chat.AppendAssistantPart(msg.Message.Presentation, chat.AssistantPart{Type: chat.AssistantPartContent, Text: "commentary"})
						msg.Message.Content = "commentary"
					case 'T':
						live.AddOrUpdateToolCall("worker", call, def, types.ToolStatusPending)
						msg.Message.Presentation = chat.AppendAssistantPart(msg.Message.Presentation, chat.AssistantPart{Type: chat.AssistantPartToolCall, ToolCallID: "call", Tool: &chat.AssistantTool{Call: call, Definition: def, Result: &result, IsError: isError}})
					}
				}
				beforeCommit := orderedTopology(live)
				live.CompleteAssistant(runtime.MessageAddedAt("s", msg, "worker", 0).(*runtime.MessageAddedEvent))
				require.Equal(t, beforeCommit, orderedTopology(live), "committed display-only pending calls must survive")
				status := types.ToolStatusCompleted
				if isError {
					status = types.ToolStatusError
				}
				live.AddToolResult(&runtime.ToolCallResponseEvent{ToolCallID: "call", Response: result, Result: &tools.ToolCallResult{Output: result, IsError: isError}}, status)
				require.Empty(t, msg.Message.ToolCalls, "harness calls stay out of provider fields")
				sess := session.New(session.WithID("s"))
				sess.AddMessage(msg)
				data, err := json.Marshal(sess)
				require.NoError(t, err)
				var persisted session.Session
				require.NoError(t, json.Unmarshal(data, &persisted))
				replay := orderedTestModel(t)
				BeginReplay(replay, PrepareReplay(&persisted), nil)
				finishPreparedReplay(t, replay)
				require.Equal(t, orderedTopology(live), orderedTopology(replay))
				require.Contains(t, ansi.Strip(live.View()), result)
				require.Contains(t, ansi.Strip(replay.View()), result)
			})
		}
	}
}

func TestOrderedAssistantReplayStaysInertAndRestoresMedia(t *testing.T) {
	for _, isError := range []bool{false, true} {
		t.Run(fmt.Sprintf("error=%v", isError), func(t *testing.T) {
			result := "stored result"
			call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check", Arguments: `{"value":"synthetic"}`}}
			sess := session.New(session.WithID("s"))
			sess.AddMessage(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, Presentation: []chat.AssistantPart{
				{Type: chat.AssistantPartReasoning, Text: "first"},
				{Type: chat.AssistantPartToolCall, ToolCallID: "call", Tool: &chat.AssistantTool{Call: call, Definition: tools.Tool{Name: "check"}, Result: &result, IsError: isError}},
				{Type: chat.AssistantPartContent, Text: "commentary"},
			}}})
			m := orderedTestModel(t)
			m.LoadFromSession(sess, map[int][]types.AssistantMedia{0: {{ID: 1, Fallback: "restored media"}}})
			require.Equal(t, "R(first;call) C(commentary)", orderedTopology(m))
			require.Len(t, m.messages[1].AssistantMedia, 1)
			require.Contains(t, ansi.Strip(m.View()), "restored media")
			require.Zero(t, m.ar.ActiveCount(), "historical results must not acquire animation leases")
		})
	}
}

func TestOrderedAssistantRemovedToolPreservesReasoningSegments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		finish chat.FinishReason
	}{{"refusal", chat.FinishReasonRefusal}, {"steering", chat.FinishReasonStop}} {
		t.Run(tc.name, func(t *testing.T) {
			m := orderedTestModel(t)
			call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check"}}
			m.AppendReasoning("worker", "first")
			m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "check"}, types.ToolStatusPending)
			m.AppendReasoning("worker", "second")
			// Refusal and steering discard the unpublished tool without coalescing text parts.
			committed := &session.Message{AgentName: "worker", Message: chat.Message{
				Role: chat.MessageRoleAssistant, ReasoningContent: "firstsecond", FinishReason: tc.finish,
				Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "first"}, {Type: chat.AssistantPartReasoning, Text: "second"}},
			}}
			m.CompleteAssistant(runtime.MessageAddedAt("s", committed, "worker", 0).(*runtime.MessageAddedEvent))
			require.Equal(t, "R(first\n\nsecond;)", orderedTopology(m))
			require.False(t, m.views[0].(*reasoningblock.Model).HasToolCall("call"))
			sess := session.New(session.WithID("s"))
			sess.AddMessage(committed)
			data, err := json.Marshal(sess)
			require.NoError(t, err)
			var persisted session.Session
			require.NoError(t, json.Unmarshal(data, &persisted))
			replay := orderedTestModel(t)
			BeginReplay(replay, PrepareReplay(&persisted), nil)
			finishPreparedReplay(t, replay)
			require.Equal(t, orderedTopology(m), orderedTopology(replay))
			require.Equal(t, ansi.Strip(m.View()), ansi.Strip(replay.View()))
		})
	}
}

func TestOrderedAssistantDisplayOnlyReasoningBoundary(t *testing.T) {
	first := &session.Message{AgentName: "worker", DisplayOnly: true, Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "prior round", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "prior round"}}}}
	live := orderedTestModel(t)
	live.AppendReasoning("worker", "prior round")
	live.CompleteAssistant(runtime.MessageAddedAt("s", first, "worker", 0).(*runtime.MessageAddedEvent))
	sess := session.New(session.WithID("s"))
	sess.AddMessage(first)
	data, err := json.Marshal(sess)
	require.NoError(t, err)
	var persisted session.Session
	require.NoError(t, json.Unmarshal(data, &persisted))
	require.True(t, persisted.Messages[0].Message.DisplayOnly)
	attached := orderedTestModel(t)
	attached.LoadFromSession(&persisted, nil)
	call := tools.ToolCall{ID: "next", Function: tools.FunctionCall{Name: "check", Arguments: `{"value":"synthetic"}`}}
	second := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{{Name: "check"}}, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartToolCall, ToolCallID: call.ID}}}}
	for _, m := range []*model{live, attached} {
		m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "check"}, types.ToolStatusRunning)
		m.CompleteAssistant(runtime.MessageAddedAt("s", second, "worker", 1).(*runtime.MessageAddedEvent))
	}
	persisted.AddMessage(second)
	data, err = json.Marshal(&persisted)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &persisted))
	replay := orderedTestModel(t)
	replay.LoadFromSession(&persisted, nil)
	for _, m := range []*model{live, attached, replay} {
		require.Equal(t, "R(prior round;) T(next)", orderedTopology(m))
		require.False(t, m.views[0].(*reasoningblock.Model).HasToolCall(call.ID))
	}
}
