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
					if userBoundary {
						want += " R(second;next)"
					} else {
						want = "R(first\n\nsecond;call,next)"
					}
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
					require.Equal(t, !toolOnly && !userBoundary, m.views[0].(*reasoningblock.Model).HasToolCall("next"))
					require.Contains(t, ansi.Strip(m.views[0].View()), "late first")
				}
			})
		}
	}
}

func TestAdjacentCommittedThinkingGroupsAndAbortedNewToolIsRemoved(t *testing.T) {
	call := tools.ToolCall{ID: "committed", Function: tools.FunctionCall{Name: "check"}}
	def := tools.Tool{Name: "check"}
	first := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "first", ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{def}, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "first"}, {Type: chat.AssistantPartToolCall, ToolCallID: call.ID}}}}
	live := orderedTestModel(t)
	live.AppendReasoning("worker", "first")
	live.AddOrUpdateToolCall("worker", call, def, types.ToolStatusRunning)
	live.CompleteAssistant(runtime.MessageAddedAt("s", first, "worker", 0).(*runtime.MessageAddedEvent))

	live.AppendReasoning("worker", "second")
	aborted := tools.ToolCall{ID: "aborted", Function: tools.FunctionCall{Name: "write_file"}}
	live.AddOrUpdateToolCall("worker", aborted, tools.Tool{Name: "write_file"}, types.ToolStatusPending)
	second := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "second", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "second"}}}}
	live.CompleteAssistant(runtime.MessageAddedAt("s", second, "worker", 2).(*runtime.MessageAddedEvent))

	require.Equal(t, 1, live.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
	block := live.views[0].(*reasoningblock.Model)
	require.Equal(t, "first\n\nsecond", block.Reasoning())
	require.True(t, block.HasToolCall(call.ID), "a tool committed by the earlier assistant record survives")
	require.False(t, block.HasToolCall(aborted.ID), "only the newly added uncommitted tool is rolled back")
}

func TestAdjacentCommittedToolThinkingOrderAcrossLiveAttachAndHistory(t *testing.T) {
	firstCall := tools.ToolCall{ID: "first-call", Function: tools.FunctionCall{Name: "first_operation", Arguments: `{"step":1}`}}
	secondCall := tools.ToolCall{ID: "second-call", Function: tools.FunctionCall{Name: "second_operation", Arguments: `{"step":2}`}}
	firstDef := tools.Tool{Name: "first_operation"}
	secondDef := tools.Tool{Name: "second_operation"}
	firstResult, secondResult := "FIRST_RESULT_MARKER", "SECOND_RESULT_MARKER"
	first := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "FIRST_REASONING_MARKER", ToolCalls: []tools.ToolCall{firstCall}, ToolDefinitions: []tools.Tool{firstDef}, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "FIRST_REASONING_MARKER"}, {Type: chat.AssistantPartToolCall, ToolCallID: firstCall.ID}}}}
	second := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "SECOND_REASONING_MARKER", ToolCalls: []tools.ToolCall{secondCall}, ToolDefinitions: []tools.Tool{secondDef}, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "SECOND_REASONING_MARKER"}, {Type: chat.AssistantPartToolCall, ToolCallID: secondCall.ID}}}}
	toolResult := func(id, output string) *session.Message {
		return &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: id, Content: output}}
	}
	appendRound := func(m *model, msg *session.Message, call tools.ToolCall, def tools.Tool, result string, position int) {
		m.AppendReasoning("worker", msg.Message.ReasoningContent)
		m.AddOrUpdateToolCall("worker", call, def, types.ToolStatusRunning)
		m.CompleteAssistant(runtime.MessageAddedAt("s", msg, "worker", position).(*runtime.MessageAddedEvent))
		m.AddToolResult(runtime.ToolCallResponse(call.ID, def, &tools.ToolCallResult{Output: result}, result, "worker").(*runtime.ToolCallResponseEvent), types.ToolStatusCompleted)
	}

	canonical := session.New(session.WithID("s"))
	canonical.AddMessage(first)
	canonical.AddMessage(toolResult(firstCall.ID, firstResult))
	canonical.AddMessage(second)
	canonical.AddMessage(toolResult(secondCall.ID, secondResult))
	before, err := json.Marshal(canonical)
	require.NoError(t, err)

	live := orderedTestModel(t)
	appendRound(live, first, firstCall, firstDef, firstResult, 0)
	appendRound(live, second, secondCall, secondDef, secondResult, 2)

	// The second turn arrives after the first record is already part of the
	// attached snapshot; it must extend that block rather than replace it.
	attached := orderedTestModel(t)
	snapshot := session.New(session.WithID("s"))
	snapshot.AddMessage(first)
	snapshot.AddMessage(toolResult(firstCall.ID, firstResult))
	attached.LoadFromSession(snapshot, nil)
	appendRound(attached, second, secondCall, secondDef, secondResult, 2)

	history := orderedTestModel(t)
	history.LoadFromSession(canonical, nil)
	for _, path := range []struct {
		name string
		m    *model
	}{{"live", live}, {"attached", attached}, {"history", history}} {
		require.Equal(t, 1, path.m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock), path.name)
		block := path.m.views[0].(*reasoningblock.Model)
		block.SetExpanded(true)
		frame := ansi.Strip(block.View())
		markers := []string{"FIRST_REASONING_MARKER", "first_operation", "FIRST_RESULT_MARKER", "SECOND_REASONING_MARKER", "second_operation", "SECOND_RESULT_MARKER"}
		previous := -1
		for _, marker := range markers {
			position := strings.Index(frame, marker)
			require.Greater(t, position, previous, "%s expanded order of %q", path.name, marker)
			previous = position
		}
	}
	after, err := json.Marshal(canonical)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after), "presentation grouping must not merge or reorder canonical records")
	require.Len(t, canonical.Messages, 4)
}

func TestLegacyAggregateReconciliationAfterToolKeepsReasoningSequence(t *testing.T) {
	t.Run("pending tool between streamed segments", func(t *testing.T) {
		m := orderedTestModel(t)
		m.AppendReasoning("worker", "A")
		call := tools.ToolCall{ID: "partial", Function: tools.FunctionCall{Name: "check"}}
		m.AddOrUpdateToolCall("worker", call, tools.Tool{Name: "check"}, types.ToolStatusPending)
		m.AppendReasoning("worker", "B")
		aggregate := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "AB"}}
		m.CompleteAssistant(runtime.MessageAddedAt("s", aggregate, "worker", 0).(*runtime.MessageAddedEvent))
		require.Equal(t, "R(A\n\nB;)", orderedTopology(m), "aggregate AB must not replace streamed segment B with the whole record")
		require.Equal(t, "A\n\nB", m.messages[0].Content)
	})

	t.Run("loaded shared block and partial tool", func(t *testing.T) {
		call := tools.ToolCall{ID: "committed", Function: tools.FunctionCall{Name: "check"}}
		def := tools.Tool{Name: "check"}
		seed := session.New(session.WithID("s"))
		seed.AddMessage(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "X", ToolCalls: []tools.ToolCall{call}, ToolDefinitions: []tools.Tool{def}, Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "X"}, {Type: chat.AssistantPartToolCall, ToolCallID: call.ID}}}})
		m := orderedTestModel(t)
		m.LoadFromSession(seed, nil)
		m.AppendReasoning("worker", "A")
		partial := tools.ToolCall{ID: "partial", Function: tools.FunctionCall{Name: "write_file"}}
		m.AddOrUpdateToolCall("worker", partial, tools.Tool{Name: "write_file"}, types.ToolStatusPending)
		aggregate := &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "C", ReasoningContent: "A"}}
		m.CompleteAssistant(runtime.MessageAddedAt("s", aggregate, "worker", 2).(*runtime.MessageAddedEvent))
		require.Equal(t, "R(X\n\nA;) C(C)", orderedTopology(m), "loaded block below the commit boundary is reconciled in place")
		require.Equal(t, 1, m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
		require.True(t, m.views[0].(*reasoningblock.Model).HasToolCall(call.ID))
		require.False(t, m.views[0].(*reasoningblock.Model).HasToolCall(partial.ID))
	})
}

func TestPresentationCommitCorrectsOnlyFinalSegmentAfterTool(t *testing.T) {
	m := orderedTestModel(t)
	firstCall := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "check"}}
	def := tools.Tool{Name: "check"}
	m.AppendReasoning("worker", "A")
	m.AddOrUpdateToolCall("worker", firstCall, def, types.ToolStatusRunning)
	m.AppendReasoning("worker", "Bpartial")
	committed := &session.Message{AgentName: "worker", Message: chat.Message{
		Role: chat.MessageRoleAssistant, ReasoningContent: "ABfinal",
		ToolCalls: []tools.ToolCall{firstCall}, ToolDefinitions: []tools.Tool{def},
		Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "A"}, {Type: chat.AssistantPartToolCall, ToolCallID: firstCall.ID}, {Type: chat.AssistantPartReasoning, Text: "Bfinal"}},
	}}
	m.CompleteAssistant(runtime.MessageAddedAt("s", committed, "worker", 0).(*runtime.MessageAddedEvent))

	require.Equal(t, "R(A\n\nBfinal;call)", orderedTopology(m))
	block := m.views[0].(*reasoningblock.Model)
	block.SetExpanded(true)
	view := ansi.Strip(block.View())
	markers := []string{"A", "check", "Bfinal"}
	previous := -1
	for _, marker := range markers {
		position := strings.Index(view, marker)
		require.Greater(t, position, previous, "commit correction preserves segment/tool order")
		previous = position
	}
	require.NotContains(t, view, "Bpartial")
	require.Equal(t, "A\n\nBfinal", m.messages[0].Content)
}

func TestPresentationCommitCorrectsExactStreamedBlockAfterSeparator(t *testing.T) {
	m := orderedTestModel(t)
	m.AppendReasoning("worker", "prior reasoning")
	m.AppendToLastMessage("worker", "separator")
	m.AppendReasoning("worker", "partial current")
	committed := &session.Message{AgentName: "worker", Message: chat.Message{
		Role: chat.MessageRoleAssistant, ReasoningContent: "current final",
		Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "current final"}},
	}}
	m.CompleteAssistant(runtime.MessageAddedAt("s", committed, "worker", 0).(*runtime.MessageAddedEvent))

	require.Equal(t, "R(prior reasoning;) C(separator) R(current final;)", orderedTopology(m))
	require.Equal(t, 3, len(m.messages))
	require.Equal(t, "prior reasoning", m.messages[0].Content, "earlier same-agent block is untouched")
	require.Equal(t, "separator", m.messages[1].Content)
	require.Equal(t, "current final", m.messages[2].Content)
}

func TestLegacyReplayGroupsReasoningWithCopySeparators(t *testing.T) {
	sess := session.New(session.WithID("legacy-group"))
	sess.AddMessage(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "first"}})
	sess.AddMessage(&session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "second"}})
	m := orderedTestModel(t)
	m.LoadFromSession(sess, nil)
	require.Equal(t, "R(first\n\nsecond;)", orderedTopology(m))
	require.Equal(t, "first\n\nsecond", m.messages[0].Content, "copy text keeps the visible segment separator")
}

func TestReplayGroupsOnlyAdjacentSameAgentThinking(t *testing.T) {
	sess := session.New(session.WithID("s"))
	for _, item := range []*session.Message{
		{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "first", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "first"}}}},
		{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, Content: "separator", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartContent, Text: "separator"}}}},
		{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "second", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "second"}}}},
		{AgentName: "researcher", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "other agent", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "other agent"}}}},
		{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ReasoningContent: "third", Presentation: []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "third"}}}},
	} {
		sess.AddMessage(item)
	}
	m := orderedTestModel(t)
	m.LoadFromSession(sess, nil)
	require.Equal(t, "R(first;) C(separator) R(second;) R(other agent;) R(third;)", orderedTopology(m))
	require.Equal(t, 4, m.MessageTypeCount(types.MessageTypeAssistantReasoningBlock))
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
