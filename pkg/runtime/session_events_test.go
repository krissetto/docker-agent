package runtime

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSessionEventHubDisconnectsSlowCompatibilitySubscriber(t *testing.T) {
	h := newSessionEventHubWithLimits(32, 8<<20)
	_, events, cancel := h.Subscribe("s", 4)
	defer cancel()
	for i := range 100 {
		h.Publish("s", AgentChoice("root", "s", strconv.Itoa(i)))
	}
	var got int
	for range events {
		got++
	}
	assert.LessOrEqual(t, got, 4)
}

func TestSessionEventHubSlowSequencedObserverGetsActionableGap(t *testing.T) {
	h := newSessionEventHubWithLimits(32, 8<<20)
	_, events, cancel, _ := h.SubscribeSequenced("s", nil, 1)
	defer cancel()
	h.Publish("s", AgentChoice("root", "s", "first"))
	h.Publish("s", AgentChoice("root", "s", "overflow"))
	var got []SequencedSessionEvent
	for event := range events {
		got = append(got, event)
	}
	require.Len(t, got, 2, "bounded event plus reserved gap slot")
	assert.Equal(t, uint64(1), got[0].Sequence)
	assert.True(t, got[1].Gap, "overflow is not unexplained EOF")
	assert.Equal(t, uint64(2), got[1].FirstAvailable)
}

func TestSessionEventHubReportsReplayGap(t *testing.T) {
	h := newSessionEventHubWithLimits(32, 8<<20)
	for i := range 100 {
		h.Publish("s", AgentChoice("root", "s", strconv.Itoa(i)))
	}
	since := uint64(0)
	seed, _, cancel, cursor := h.SubscribeSequenced("s", &since, 4)
	defer cancel()
	require.NotEmpty(t, seed)
	assert.True(t, seed[0].Gap)
	assert.Equal(t, uint64(69), seed[0].FirstAvailable)
	assert.Len(t, seed, 33)
	assert.Equal(t, uint64(100), cursor)
}

func TestSessionEventHubReportsGapWhenReplayIsEmpty(t *testing.T) {
	h := newSessionEventHubWithLimits(10, 1)
	h.Publish("s", AgentChoice("root", "s", "too large"))
	since := uint64(0)
	seed, _, cancel, cursor := h.SubscribeSequenced("s", &since, 1)
	defer cancel()
	require.Len(t, seed, 1)
	assert.True(t, seed[0].Gap)
	assert.Equal(t, uint64(2), seed[0].FirstAvailable)
	assert.Equal(t, uint64(1), cursor)
}

func TestSessionEventHubReplaysAfterDisconnect(t *testing.T) {
	h := newSessionEventHubWithLimits(128, 8<<20)
	_, events, cancel, cursor := h.SubscribeSequenced("s", nil, 2)
	for i := range 10 {
		h.Publish("s", AgentChoice("root", "s", strconv.Itoa(i)))
	}
	for range events {
	}
	cancel()
	seed, _, cancel2, _ := h.SubscribeSequenced("s", &cursor, 2)
	defer cancel2()
	require.Len(t, seed, 10)
	for i, item := range seed {
		assert.False(t, item.Gap)
		assert.Equal(t, uint64(i+1), item.Sequence)
		assert.Equal(t, strconv.Itoa(i), item.Event.(*AgentChoiceEvent).Content)
	}
}

func TestSessionEventHubSeedsInflightAssistant(t *testing.T) {
	h := newSessionEventHubWithLimits(defaultSessionEventReplayCapacity, 8<<20)
	h.Publish("s1", AgentChoiceReasoning("planner", "s1", "thinking… "))
	h.Publish("s1", AgentChoice("planner", "s1", "Hello, "))
	h.Publish("s1", AgentChoice("planner", "s1", "wor"))
	seed, ch, cancel := h.Subscribe("s1", 16)
	defer cancel()
	require.Len(t, seed, 2)
	assert.Equal(t, "thinking… ", seed[0].(*AgentChoiceReasoningEvent).Content)
	assert.Equal(t, "Hello, wor", seed[1].(*AgentChoiceEvent).Content)
	h.Publish("s1", AgentChoice("planner", "s1", "ld!"))
	assert.Equal(t, "ld!", (<-ch).(*AgentChoiceEvent).Content)
	h.Publish("s1", &MessageAddedEvent{SessionID: "s1"})
	seed2, _, cancel2 := h.Subscribe("s1", 16)
	defer cancel2()
	assert.Empty(t, seed2)
}

func TestSessionEventHubResetsInflightOnBoundaries(t *testing.T) {
	for _, boundary := range []Event{&StreamStoppedEvent{}, &ErrorEvent{}, UserMessage("hi", "s1", nil)} {
		h := newSessionEventHubWithLimits(defaultSessionEventReplayCapacity, 8<<20)
		h.Publish("s1", AgentChoice("planner", "s1", "partial"))
		h.Publish("s1", boundary)
		seed, _, cancel := h.Subscribe("s1", 1)
		assert.Empty(t, seed)
		cancel()
	}
}

func TestSessionEventHubSeedsLiveRun(t *testing.T) {
	h := newSessionEventHubWithLimits(defaultSessionEventReplayCapacity, 8<<20)
	h.Publish("s1", StreamStarted("s1", "coder"))
	h.Publish("s1", AgentChoice("coder", "s1", "Working…"))
	seed, _, cancel := h.Subscribe("s1", 16)
	cancel()
	require.Len(t, seed, 2)
	assert.IsType(t, &StreamStartedEvent{}, seed[0])
	h.Publish("s1", StreamStopped("s1", "coder", ""))
	seed, _, cancel = h.Subscribe("s1", 16)
	cancel()
	assert.Empty(t, seed)
}

func TestSessionEventHubSeedsActiveTools(t *testing.T) {
	h := newSessionEventHubWithLimits(1, 8)
	definition := tools.Tool{Name: "check", Parameters: map[string]any{"nested": []any{"original"}}, Metadata: map[string]string{"key": "original"}}
	call := tools.ToolCall{ID: "first", Type: "function", Function: tools.FunctionCall{Name: "check", Arguments: strings.Repeat("prefix", 100)}}
	h.Publish("s", StreamStarted("s", "worker"))
	h.Publish("s", PartialToolCall(call, definition, "worker"))
	delta := call
	delta.Function.Arguments = "suffix"
	h.Publish("s", PartialToolCall(delta, tools.Tool{}, "worker"))
	seed, _, cancel, cursor := h.SubscribeSequenced("s", nil, 8)
	cancel()
	require.Len(t, seed, 2)
	partial := seed[1].Event.(*PartialToolCallEvent)
	assert.Equal(t, call.Function.Arguments+"suffix", partial.ToolCall.Function.Arguments)
	assert.Zero(t, seed[1].Sequence)
	assert.EqualValues(t, 3, cursor)
	partial.ToolCall.Function.Arguments = "mutated"
	partial.ToolDefinition.Parameters.(map[string]any)["nested"].([]any)[0] = "mutated"
	partial.ToolDefinition.Metadata["key"] = "mutated"
	seed, _, cancel, _ = h.SubscribeSequenced("s", nil, 8)
	cancel()
	partial = seed[1].Event.(*PartialToolCallEvent)
	assert.Equal(t, call.Function.Arguments+"suffix", partial.ToolCall.Function.Arguments)
	assert.Equal(t, "original", partial.ToolDefinition.Parameters.(map[string]any)["nested"].([]any)[0])
	assert.Equal(t, "original", partial.ToolDefinition.Metadata["key"])
	since := uint64(1)
	replay, _, cancel, _ := h.SubscribeSequenced("s", &since, 8)
	cancel()
	require.Len(t, replay, 1)
	assert.True(t, replay[0].Gap, "journal eviction cannot erase authoritative arguments")

	call.Function.Arguments = "canonical"
	h.Publish("s", ToolCall(call, definition, "worker"))
	second := call
	second.ID = "second"
	h.Publish("s", ToolCall(second, definition, "worker"))
	h.Publish("s", ToolCallOutput(call.ID, definition, "12345", "worker"))
	h.Publish("s", ToolCallOutput(second.ID, definition, "ééé", "worker"))
	seed, _, cancel, _ = h.SubscribeSequenced("s", nil, 8)
	cancel()
	require.Len(t, seed, 5)
	assert.Equal(t, "canonical", seed[1].Event.(*ToolCallEvent).ToolCall.Function.Arguments)
	assert.Equal(t, "12345", seed[2].Event.(*ToolCallOutputEvent).Output)
	assert.Equal(t, "second", seed[3].Event.(*ToolCallEvent).ToolCall.ID)
	assert.Contains(t, seed[4].Event.(*ToolCallOutputEvent).Output, "é\n[Earlier tool output truncated")
	assert.Equal(t, 7, h.outputBytes["s"])
	h.Publish("s", ToolCallResponse(call.ID, definition, &tools.ToolCallResult{}, "", "worker"))
	assert.Equal(t, 2, h.outputBytes["s"])
	h.Publish("s", ToolCallOutput(second.ID, definition, "not contiguous", "worker"))
	assert.Equal(t, 2, h.outputBytes["s"], "do not join retained output across an omitted section")
	third := call
	third.ID = "third"
	h.Publish("s", ToolCall(third, definition, "worker"))
	h.Publish("s", ToolCallOutput(third.ID, definition, "123456", "worker"))
	assert.Equal(t, 8, h.outputBytes["s"], "completed calls release budget for other calls")
	h.Publish("s", ToolCallResponse(third.ID, definition, &tools.ToolCallResult{}, "", "worker"))
	h.Publish("s", ToolCallResponse(second.ID, definition, &tools.ToolCallResult{}, "", "worker"))
	assert.Empty(t, h.activeTools)
	assert.Empty(t, h.outputBytes)
}

func TestSessionEventHubActiveToolCleanup(t *testing.T) {
	for _, boundary := range []string{"stop", "cancel", "delete", "close"} {
		t.Run(boundary, func(t *testing.T) {
			h := newSessionEventHubWithLimits(32, 32)
			h.Publish("s", StreamStarted("s", "worker"))
			h.Publish("s", ToolCall(tools.ToolCall{ID: "call"}, tools.Tool{}, "worker"))
			h.Publish("s", ToolCallOutput("call", tools.Tool{}, "output", "worker"))
			switch boundary {
			case "stop", "cancel":
				h.Publish("s", StreamStopped("s", "worker", boundary))
			case "delete":
				h.Delete("s")
			case "close":
				h.Close()
			}
			assert.Empty(t, h.activeTools)
			assert.Empty(t, h.outputBytes)
		})
	}
}

func TestSessionEventHubCommittedPartialDoesNotDuplicateSnapshotArguments(t *testing.T) {
	h := newSessionEventHubWithLimits(32, 1024)
	call := tools.ToolCall{ID: "call", Function: tools.FunctionCall{Arguments: "prefix"}}
	h.Publish("s", PartialToolCall(call, tools.Tool{}, "worker"))
	h.Publish("s", &MessageAddedEvent{Message: &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}}})
	seed, _, cancel := h.Subscribe("s", 8)
	defer cancel()
	assert.Empty(t, seed)
}

func TestSessionEventHubParallelPartialsAndCursorReplay(t *testing.T) {
	h := newSessionEventHubWithLimits(32, 4096)
	for _, id := range []string{"first", "second"} {
		h.Publish("s", PartialToolCall(tools.ToolCall{ID: id, Type: "function", Function: tools.FunctionCall{Name: "check", Arguments: id}}, tools.Tool{}, "worker"))
	}
	for _, id := range []string{"second", "first"} {
		h.Publish("s", PartialToolCall(tools.ToolCall{ID: id, Function: tools.FunctionCall{Arguments: " suffix"}}, tools.Tool{}, "worker"))
	}
	seed, _, cancel, cursor := h.SubscribeSequenced("s", nil, 8)
	cancel()
	require.Len(t, seed, 2)
	for i, id := range []string{"first", "second"} {
		call := seed[i].Event.(*PartialToolCallEvent).ToolCall
		assert.Equal(t, id, call.ID)
		assert.Equal(t, "check", call.Function.Name)
		assert.Equal(t, tools.ToolType("function"), call.Type)
		assert.Equal(t, id+" suffix", call.Function.Arguments)
	}
	since := uint64(2)
	replay, _, cancel, head := h.SubscribeSequenced("s", &since, 8)
	cancel()
	assert.Equal(t, cursor, head)
	require.Len(t, replay, 2)
	for i, item := range replay {
		assert.EqualValues(t, i+3, item.Sequence)
		assert.Equal(t, " suffix", item.Event.(*PartialToolCallEvent).ToolCall.Function.Arguments, "cursor replay contains deltas, never accumulated seeds")
	}
}

func TestSessionEventHubZeroOutputBudgetIsExplicit(t *testing.T) {
	h := newSessionEventHubWithLimits(0, 0)
	h.Publish("s", ToolCall(tools.ToolCall{ID: "call"}, tools.Tool{}, "worker"))
	h.Publish("s", ToolCallOutput("call", tools.Tool{}, "cannot retain", "worker"))
	seed, _, cancel := h.Subscribe("s", 8)
	defer cancel()
	require.Len(t, seed, 2)
	assert.Contains(t, seed[1].(*ToolCallOutputEvent).Output, "[Earlier tool output truncated")
	assert.NotContains(t, seed[1].(*ToolCallOutputEvent).Output, "cannot retain")
	assert.Zero(t, h.outputBytes["s"])
}

func TestSessionEventHubCopiesTypedToolSchema(t *testing.T) {
	h := newSessionEventHubWithLimits(32, 4096)
	definition := tools.Tool{Name: "check", Parameters: tools.MustSchemaFor[struct {
		Marker string `json:"marker"`
	}]()}
	h.Publish("s", PartialToolCall(tools.ToolCall{ID: "call"}, definition, "worker"))
	seed, _, cancel := h.Subscribe("s", 8)
	cancel()
	schema := seed[0].(*PartialToolCallEvent).ToolDefinition.Parameters.(map[string]any)
	schema["properties"].(map[string]any)["marker"].(map[string]any)["type"] = "mutated"
	seed, _, cancel = h.Subscribe("s", 8)
	defer cancel()
	schema = seed[0].(*PartialToolCallEvent).ToolDefinition.Parameters.(map[string]any)
	assert.Equal(t, "string", schema["properties"].(map[string]any)["marker"].(map[string]any)["type"])
}
