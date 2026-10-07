package runtime

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSessionEventHubLiveSeedPresentationOrder(t *testing.T) {
	for _, order := range []string{"RT", "RCT", "RTC", "TR", "RCRT", "RTR", "T", "RRCC", "TCRT"} {
		t.Run(order, func(t *testing.T) {
			h := newSessionEventHubWithLimits(1, 64)
			_, live, cancel := h.Subscribe("s", 16)
			defer cancel()
			h.Publish("s", StreamStarted("s", "worker"))
			var want []string
			toolSeen := false
			for i, kind := range order {
				text := fmt.Sprint(i)
				switch kind {
				case 'R':
					h.Publish("s", AgentChoiceReasoning("worker", "s", text))
				case 'C':
					h.Publish("s", AgentChoice("worker", "s", text))
				case 'T':
					h.Publish("s", ToolCall(tools.ToolCall{ID: "call"}, tools.Tool{}, "worker"))
				}
				if kind == 'T' {
					if !toolSeen {
						want = append(want, "T:call")
						toolSeen = true
					}
				} else if n := len(want); n > 0 && strings.HasPrefix(want[n-1], string(kind)+":") {
					want[n-1] += text
				} else {
					want = append(want, string(kind)+":"+text)
				}
			}
			want = append([]string{"start"}, want...)
			seed, _, detach := h.Subscribe("s", 16)
			detach()
			require.Equal(t, want, orderingSeedLabels(t, seed))
			sequenced, _, detach, cursor := h.SubscribeSequenced("s", nil, 16)
			detach()
			require.EqualValues(t, len(order)+1, cursor)
			var events []Event
			for _, item := range sequenced {
				require.Zero(t, item.Sequence)
				events = append(events, item.Event)
			}
			require.Equal(t, want, orderingSeedLabels(t, events))
			var original []Event
			for range len(order) + 1 {
				original = append(original, <-live)
			}
			labels := orderingSeedLabels(t, original)
			require.Equal(t, "start", labels[0])
			for i, kind := range order {
				if kind == 'T' {
					require.Equal(t, "T:call", labels[i+1])
				} else {
					require.Equal(t, fmt.Sprintf("%c:%d", kind, i), labels[i+1])
				}
			}
		})
	}
}

func TestSessionEventHubLiveSeedToolSlotsAcrossBoundaries(t *testing.T) {
	for _, boundary := range []Event{
		UserMessage("next", "s", nil),
		MessageAdded("s", &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant}}, "worker"),
	} {
		t.Run(fmt.Sprintf("%T", boundary), func(t *testing.T) {
			h := newSessionEventHubWithLimits(1, 1024)
			h.Publish("s", AgentChoiceReasoning("worker", "s", "committed"))
			h.Publish("s", ToolCall(tools.ToolCall{ID: "old"}, tools.Tool{}, "worker"))
			h.Publish("s", boundary)
			h.Publish("s", AgentChoiceReasoning("worker", "s", "new"))
			h.Publish("s", PartialToolCall(tools.ToolCall{ID: "fresh", Function: tools.FunctionCall{Arguments: "prefix"}}, tools.Tool{}, "worker"))
			h.Publish("s", AgentChoice("worker", "s", "commentary"))
			h.Publish("s", PartialToolCall(tools.ToolCall{ID: "fresh", Function: tools.FunctionCall{Arguments: "suffix"}}, tools.Tool{}, "worker"))
			h.Publish("s", ToolCall(tools.ToolCall{ID: "old", Function: tools.FunctionCall{Arguments: "updated"}}, tools.Tool{}, "worker"))
			seed, _, cancel := h.Subscribe("s", 16)
			cancel()
			require.Equal(t, []string{"T:old", "R:new", "P:fresh", "C:commentary"}, orderingSeedLabels(t, seed))
			require.Equal(t, "updated", seed[0].(*ToolCallEvent).ToolCall.Function.Arguments)
			require.Equal(t, "prefixsuffix", seed[2].(*PartialToolCallEvent).ToolCall.Function.Arguments)
			h.Publish("s", ToolCallResponse("old", tools.Tool{}, &tools.ToolCallResult{}, "done", "worker"))
			seed, _, cancel = h.Subscribe("s", 16)
			cancel()
			require.Equal(t, []string{"R:new", "P:fresh", "C:commentary"}, orderingSeedLabels(t, seed))
			h.Publish("s", ToolCallResponse("fresh", tools.Tool{}, &tools.ToolCallResult{}, "done", "worker"))
			h.Publish("s", AgentChoiceReasoning("worker", "s", "last"))
			seed, _, cancel = h.Subscribe("s", 16)
			cancel()
			require.Equal(t, []string{"R:new", "T:fresh", "D:fresh:done", "C:commentary", "R:last"}, orderingSeedLabels(t, seed))
		})
	}
}

func TestSessionEventHubOrderedSeedByteLimit(t *testing.T) {
	h := newSessionEventHubWithLimits(0, 0)
	h.Publish("s", AgentChoiceReasoning("worker", "s", strings.Repeat("r", maxSessionEventSubscriberBytes/2)))
	h.Publish("s", AgentChoice("worker", "s", strings.Repeat("c", maxSessionEventSubscriberBytes/2)))
	require.False(t, h.liveSeedFitsLocked("s"), "serialized event overhead remains subject to the byte limit")
	seed, events, cancel, _ := h.SubscribeSequenced("s", nil, 1)
	defer cancel()
	require.Empty(t, seed)
	require.True(t, (<-events).Gap)
}

func orderingSeedLabels(t *testing.T, events []Event) []string {
	t.Helper()
	var labels []string
	for _, event := range events {
		switch e := event.(type) {
		case *StreamStartedEvent:
			labels = append(labels, "start")
		case *AgentChoiceReasoningEvent:
			labels = append(labels, "R:"+e.Content)
		case *AgentChoiceEvent:
			labels = append(labels, "C:"+e.Content)
		case *ToolCallEvent:
			labels = append(labels, "T:"+e.ToolCall.ID)
		case *ToolCallResponseEvent:
			labels = append(labels, "D:"+e.ToolCallID+":"+e.Response)
		case *PartialToolCallEvent:
			labels = append(labels, "P:"+e.ToolCall.ID)
		default:
			t.Fatalf("unexpected seed event %T", event)
		}
	}
	return labels
}

func TestSessionEventHubHarnessCompletedToolSeeds(t *testing.T) {
	h := newSessionEventHubWithLimits(1, 64)
	h.Publish("s", AgentChoiceReasoning("worker", "s", "first"))
	definition := tools.Tool{Name: "check", Metadata: map[string]string{"key": "original"}}
	h.Publish("s", ToolCall(tools.ToolCall{ID: "call", Function: tools.FunctionCall{Arguments: "{}"}}, definition, "worker"))
	h.Publish("s", AgentChoice("worker", "s", "middle"))
	h.Publish("s", ToolCallResponse("call", definition, &tools.ToolCallResult{IsError: true}, "failed", "worker"))
	h.Publish("s", MessageAdded("s", &session.Message{Message: chat.Message{Role: chat.MessageRoleTool, ToolCallID: "call"}}, "worker"))
	h.Publish("s", AgentChoiceReasoning("worker", "s", "last"))
	seed, _, cancel := h.Subscribe("s", 16)
	cancel()
	require.Equal(t, []string{"R:first", "T:call", "D:call:failed", "C:middle", "R:last"}, orderingSeedLabels(t, seed))
	require.True(t, seed[2].(*ToolCallResponseEvent).Result.IsError)
	require.Equal(t, "{}", seed[1].(*ToolCallEvent).ToolCall.Function.Arguments)
	seed[1].(*ToolCallEvent).ToolDefinition.Metadata["key"] = "mutated"
	seed[2].(*ToolCallResponseEvent).ToolDefinition.Metadata["key"] = "also mutated"
	seed, _, cancel = h.Subscribe("s", 16)
	cancel()
	require.Equal(t, "original", seed[1].(*ToolCallEvent).ToolDefinition.Metadata["key"])
	require.Equal(t, "original", seed[2].(*ToolCallResponseEvent).ToolDefinition.Metadata["key"])
	sequenced, _, cancel, _ := h.SubscribeSequenced("s", nil, 16)
	cancel()
	require.Len(t, sequenced, 5)
	for _, item := range sequenced {
		envelope := sessionEnvelope("s", item)
		require.True(t, envelope.IsLiveSeed(), "completed harness seeds must survive public/remote validation")
	}
	h.Publish("s", MessageAdded("s", &session.Message{Message: chat.Message{Role: chat.MessageRoleAssistant}}, "worker"))
	seed, _, cancel = h.Subscribe("s", 16)
	cancel()
	require.Empty(t, seed)
}

func TestSessionEventHubResolvedSeedByteLimit(t *testing.T) {
	h := newSessionEventHubWithLimits(0, 0)
	for _, id := range []string{"first", "second"} {
		h.Publish("s", ToolCall(tools.ToolCall{ID: id}, tools.Tool{}, "worker"))
		h.Publish("s", ToolCallResponse(id, tools.Tool{}, nil, strings.Repeat("x", maxSessionEventSubscriberBytes/2), "worker"))
	}
	require.Empty(t, h.activeTools["s"])
	require.LessOrEqual(t, h.inflight["s"].resolvedBytes, maxSessionEventSubscriberBytes)
	require.False(t, h.liveSeedFitsLocked("s"))
	seed, events, cancel, _ := h.SubscribeSequenced("s", nil, 1)
	cancel()
	require.Empty(t, seed)
	require.True(t, (<-events).Gap)
	h.Publish("s", UserMessage("next", "s", nil))
	require.True(t, h.liveSeedFitsLocked("s"))
}

func TestSessionEventResponseLiveSeedValidity(t *testing.T) {
	envelope := SessionEvent{Version: 1, SessionID: "s", TranscriptPosition: -1, Event: ToolCallResponse("call", tools.Tool{}, nil, "", "worker")}
	require.True(t, envelope.IsLiveSeed())
	envelope.Sequence = 1
	require.False(t, envelope.IsLiveSeed())
	envelope.Sequence = 0
	envelope.Event = ToolCallResponse("", tools.Tool{}, nil, "result", "worker")
	require.False(t, envelope.IsLiveSeed())
}

func TestSessionEventHubPartialOnlyCompletionAndSerializedToolCommit(t *testing.T) {
	h := newSessionEventHubWithLimits(1, 64)
	h.Publish("s", PartialToolCall(tools.ToolCall{ID: "call", Function: tools.FunctionCall{Arguments: "{}"}}, tools.Tool{}, "worker"))
	h.Publish("s", ToolCallResponse("call", tools.Tool{}, nil, "done", "worker"))
	h.Publish("s", &MessageAddedEvent{MessageRole: chat.MessageRoleTool})
	seed, _, cancel := h.Subscribe("s", 16)
	cancel()
	require.Equal(t, []string{"T:call", "D:call:done"}, orderingSeedLabels(t, seed))
	require.Equal(t, "worker", seed[0].(*ToolCallEvent).AgentName)
	require.Equal(t, "worker", seed[1].(*ToolCallResponseEvent).AgentName)
}

func TestSessionEventHubCommittedPartialExecutionIsNotHarnessSeed(t *testing.T) {
	h := newSessionEventHubWithLimits(1, 64)
	call := tools.ToolCall{ID: "old", Function: tools.FunctionCall{Arguments: "{}"}}
	h.Publish("s", PartialToolCall(call, tools.Tool{}, "worker"))
	h.Publish("s", MessageAdded("s", &session.Message{AgentName: "worker", Message: chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{call}}}, "worker"))
	seed, _, cancel := h.Subscribe("s", 16)
	cancel()
	require.Empty(t, seed, "snapshot owns committed arguments")
	h.Publish("s", AgentChoiceReasoning("worker", "s", "new"))
	h.Publish("s", ToolCall(call, tools.Tool{}, "worker"))
	h.Publish("s", AgentChoice("worker", "s", "commentary"))
	seed, _, cancel = h.Subscribe("s", 16)
	cancel()
	require.Equal(t, []string{"T:old", "R:new", "C:commentary"}, orderingSeedLabels(t, seed))
	h.Publish("s", ToolCallResponse("old", tools.Tool{}, nil, "done", "worker"))
	h.Publish("s", &MessageAddedEvent{MessageRole: chat.MessageRoleTool})
	seed, _, cancel = h.Subscribe("s", 16)
	cancel()
	require.Equal(t, []string{"R:new", "C:commentary"}, orderingSeedLabels(t, seed))
}
