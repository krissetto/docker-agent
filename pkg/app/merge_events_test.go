package app

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestMergeEventsConcatenatesAgentChoiceContent(t *testing.T) {
	t.Parallel()

	a := &App{}
	chunks := []string{"Hello ", "streaming ", "world", "!"}
	events := make([]any, 0, len(chunks))
	for _, c := range chunks {
		events = append(events, &runtime.AgentChoiceEvent{
			Content:      c,
			AgentContext: runtime.AgentContext{AgentName: "agent-a"},
		})
	}

	merged := a.mergeEvents(events)

	assert.Len(t, merged, 1)
	got, ok := merged[0].(*runtime.AgentChoiceEvent)
	assert.True(t, ok)
	assert.Equal(t, "Hello streaming world!", got.Content)
}

func TestMergeEventsKeepsBoundaryBetweenAgents(t *testing.T) {
	t.Parallel()

	a := &App{}
	events := []any{
		&runtime.AgentChoiceEvent{Content: "a1", AgentContext: runtime.AgentContext{AgentName: "agent-a"}},
		&runtime.AgentChoiceEvent{Content: "a2", AgentContext: runtime.AgentContext{AgentName: "agent-a"}},
		&runtime.AgentChoiceEvent{Content: "b1", AgentContext: runtime.AgentContext{AgentName: "agent-b"}},
		&runtime.AgentChoiceEvent{Content: "a3", AgentContext: runtime.AgentContext{AgentName: "agent-a"}},
	}

	merged := a.mergeEvents(events)

	assert.Len(t, merged, 3)
	assert.Equal(t, "a1a2", merged[0].(*runtime.AgentChoiceEvent).Content)
	assert.Equal(t, "b1", merged[1].(*runtime.AgentChoiceEvent).Content)
	assert.Equal(t, "a3", merged[2].(*runtime.AgentChoiceEvent).Content)
}

func TestMergeEventsConcatenatesPartialToolCallArguments(t *testing.T) {
	t.Parallel()

	a := &App{}
	events := []any{
		&runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Arguments: `{"a"`},
			},
		},
		&runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Name: "shell", Arguments: `:1`},
			},
		},
		&runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Arguments: `}`},
			},
		},
	}

	merged := a.mergeEvents(events)

	assert.Len(t, merged, 1)
	got := merged[0].(*runtime.PartialToolCallEvent)
	assert.Equal(t, `{"a":1}`, got.ToolCall.Function.Arguments)
	assert.Equal(t, "shell", got.ToolCall.Function.Name)
}

func TestMergeEventsConcatenatesToolCallOutput(t *testing.T) {
	t.Parallel()

	a := &App{}
	events := []any{
		&runtime.ToolCallOutputEvent{ToolCallID: "call-1", Output: "line 1\n"},
		&runtime.ToolCallOutputEvent{ToolCallID: "call-1", Output: "line 2\n"},
		&runtime.ToolCallOutputEvent{ToolCallID: "call-2", Output: "other\n"},
	}

	merged := a.mergeEvents(events)

	assert.Len(t, merged, 2)
	assert.Equal(t, "line 1\nline 2\n", merged[0].(*runtime.ToolCallOutputEvent).Output)
	assert.Equal(t, "other\n", merged[1].(*runtime.ToolCallOutputEvent).Output)
}

func TestMergeEventsCanonicalDeltas(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"text", "reasoning", "arguments", "output"} {
		t.Run(kind, func(t *testing.T) {
			a := &App{}
			first := canonicalMergeTestEvent(kind, "FIRST")
			second := canonicalMergeTestEvent(kind, "SECOND")
			last := canonicalMergeTestEvent(kind, "FINAL")
			last.Projection = &PresentationState{Status: runtime.SessionStatus{SessionID: "session", Pending: 3}}
			if kind == "arguments" {
				second.Event.(*runtime.PartialToolCallEvent).ToolCall.Function.Name = "shell"
				second.Event.(*runtime.PartialToolCallEvent).ToolDefinition = &tools.Tool{Name: "shell"}
			}
			inputs := []any{first, second, last}
			merged := a.mergeEvents(inputs)
			require.Len(t, merged, 1)
			got := merged[0].(SessionEventMsg)
			assert.Equal(t, "FIRSTSECONDFINAL", canonicalMergeTestContent(got.Event))
			assert.Same(t, last.Projection, got.Projection)
			metadata := got
			metadata.Event = last.Event
			assert.Equal(t, last, metadata)
			assert.Equal(t, "FIRST", canonicalMergeTestContent(first.Event))
			assert.Equal(t, "SECOND", canonicalMergeTestContent(second.Event))
			assert.Equal(t, "FINAL", canonicalMergeTestContent(last.Event))
			assert.NotSame(t, first.Event, got.Event)
			assert.NotSame(t, last.Event, got.Event)
			assert.True(t, a.shouldThrottle(first))
			assert.True(t, a.shouldThrottle(first.Event))
			if kind == "arguments" {
				delta := got.Event.(*runtime.PartialToolCallEvent)
				assert.Equal(t, "shell", delta.ToolCall.Function.Name)
				require.NotNil(t, delta.ToolDefinition)
				assert.Equal(t, "shell", delta.ToolDefinition.Name)
				assert.Empty(t, first.Event.(*runtime.PartialToolCallEvent).ToolCall.Function.Name)
				assert.Nil(t, first.Event.(*runtime.PartialToolCallEvent).ToolDefinition)
			}
			if scoped, ok := got.Event.(runtime.SessionScoped); ok {
				assert.Equal(t, "session", scoped.GetSessionID())
			}
			assert.Equal(t, first.Event.GetAgentName(), got.Event.GetAgentName())
			assert.Equal(t, merged, a.mergeEvents(inputs), "merging does not consume or mutate inputs")
			raw := a.mergeEvents([]any{first.Event, second.Event, last.Event})
			require.Len(t, raw, 1)
			assert.Equal(t, got.Event, raw[0], "raw and wrapped deltas preserve the same payload")
		})
	}
}

func TestMergeEventsCanonicalIdentityBoundaries(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"text", "reasoning", "arguments", "output"} {
		for _, identity := range []string{"origin", "turn", "epoch", "seed", "agent", "session-or-tool", "event-type", "raw"} {
			t.Run(kind+"/"+identity, func(t *testing.T) {
				first := canonicalMergeTestEvent(kind, "FIRST")
				second := canonicalMergeTestEvent(kind, "SECOND")
				third := canonicalMergeTestEvent(kind, "FINAL")
				switch identity {
				case "origin":
					second.OriginSessionID = "other"
				case "turn":
					second.TurnID = "other"
				case "epoch":
					second.Epoch++
				case "seed":
					second.Seed = !second.Seed
				}
				switch event := second.Event.(type) {
				case *runtime.AgentChoiceEvent:
					switch identity {
					case "agent":
						event.AgentName = "other"
					case "session-or-tool":
						event.SessionID = "other"
					case "event-type":
						event.Type = "other"
					}
				case *runtime.AgentChoiceReasoningEvent:
					switch identity {
					case "agent":
						event.AgentName = "other"
					case "session-or-tool":
						event.SessionID = "other"
					case "event-type":
						event.Type = "other"
					}
				case *runtime.PartialToolCallEvent:
					switch identity {
					case "agent":
						event.AgentName = "other"
					case "session-or-tool":
						event.ToolCall.ID = "other"
					case "event-type":
						event.ToolCall.Type = "other"
					}
				case *runtime.ToolCallOutputEvent:
					switch identity {
					case "agent":
						event.AgentName = "other"
					case "session-or-tool":
						event.ToolCallID = "other"
					case "event-type":
						event.Type = "other"
					}
				}
				var middle any = second
				if identity == "raw" {
					middle = second.Event
				}
				inputs := []any{first, middle, third}
				a := &App{}
				assert.Equal(t, inputs, a.mergeEvents(inputs))
				if identity == "agent" || identity == "session-or-tool" || identity == "event-type" {
					raw := []any{first.Event, second.Event, third.Event}
					assert.Equal(t, raw, a.mergeEvents(raw), "raw deltas retain identity boundaries too")
				}
			})
		}
	}
}

func TestMergeEventsCanonicalBarriers(t *testing.T) {
	t.Parallel()

	barriers := map[string]runtime.Event{
		"start":       runtime.StreamStarted("session", "agent"),
		"stop":        runtime.StreamStopped("session", "agent", "done"),
		"error":       runtime.Error("failure"),
		"reset":       &SessionResetEvent{Snapshot: runtime.SessionSnapshot{Cursor: 123}},
		"interaction": &runtime.MaxIterationsReachedEvent{SessionID: "session", RequestID: "interaction"},
		"resolution":  &runtime.InteractionResolvedEvent{SessionID: "session", InteractionID: "interaction"},
	}
	for name, barrier := range barriers {
		t.Run(name, func(t *testing.T) {
			a := &App{}
			before := canonicalMergeTestEvent("text", "before")
			after := canonicalMergeTestEvent("text", "after")
			wrapped := before
			wrapped.Event = barrier
			assert.False(t, a.shouldThrottle(wrapped))
			assert.False(t, a.shouldThrottle(barrier))
			for _, middle := range []any{wrapped, barrier} {
				merged := a.mergeEvents([]any{before, before, middle, after, after})
				require.Len(t, merged, 3)
				assert.Equal(t, "beforebefore", canonicalMergeTestContent(merged[0].(SessionEventMsg).Event))
				assert.Equal(t, middle, merged[1], "barrier metadata and reset cursor are untouched")
				assert.Equal(t, "afterafter", canonicalMergeTestContent(merged[2].(SessionEventMsg).Event))
			}
		})
	}

	a := &App{}
	var mixed []any
	for _, kind := range []string{"text", "reasoning", "arguments", "output", "text"} {
		mixed = append(mixed, canonicalMergeTestEvent(kind, kind))
	}
	assert.Equal(t, mixed, a.mergeEvents(mixed), "different delta types are ordered boundaries")
}

func TestThrottleEventsCanonicalBurst(t *testing.T) {
	t.Parallel()

	const chunks = 1500
	for _, boundary := range []string{"status", "close"} {
		t.Run(boundary, func(t *testing.T) {
			// A long window makes the barrier, rather than scheduling, flush the burst.
			a := &App{throttleDuration: time.Hour}
			in := make(chan any, chunks+4)
			in <- canonicalMergeTestEvent("text", "FIRST")
			for range chunks {
				in <- canonicalMergeTestEvent("text", "chunk")
			}
			in <- canonicalMergeTestEvent("text", "FINAL")
			stop := canonicalMergeTestEvent("text", "")
			stop.Event = runtime.StreamStopped("session", "agent", "done")
			if boundary == "status" {
				in <- stop
			} else {
				close(in)
			}
			out := a.throttleEvents(t.Context(), in)
			var got []any
			timeout := time.After(time.Second)
			for {
				select {
				case event, ok := <-out:
					if !ok {
						want := 1
						if boundary == "status" {
							want++
						}
						require.Len(t, got, want)
						assert.Equal(t, "FIRST"+strings.Repeat("chunk", chunks)+"FINAL", canonicalMergeTestContent(got[0].(SessionEventMsg).Event))
						if boundary == "status" {
							assert.Equal(t, stop, got[1])
						}
						return
					}
					got = append(got, event)
					if boundary == "status" && len(got) == 2 {
						close(in)
					}
				case <-timeout:
					t.Fatal("canonical burst did not flush")
				}
			}
		})
	}
}

func TestThrottleEventsCanonicalTimer(t *testing.T) {
	t.Parallel()

	a := &App{throttleDuration: time.Millisecond}
	in := make(chan any, 1)
	first := canonicalMergeTestEvent("text", "FIRST")
	in <- first
	out := a.throttleEvents(t.Context(), in)
	select {
	case got := <-out:
		assert.Equal(t, first, got, "live output is visible before any stop or close")
	case <-time.After(time.Second):
		t.Fatal("canonical live delta did not flush on timer")
	}
}

func canonicalMergeTestEvent(kind, content string) SessionEventMsg {
	var event runtime.Event
	context := runtime.AgentContext{AgentName: "agent"}
	switch kind {
	case "text":
		event = &runtime.AgentChoiceEvent{Type: "agent_choice", SessionID: "session", Content: content, AgentContext: context}
	case "reasoning":
		event = &runtime.AgentChoiceReasoningEvent{Type: "agent_choice_reasoning", SessionID: "session", Content: content, AgentContext: context}
	case "arguments":
		event = &runtime.PartialToolCallEvent{
			Type:         "partial_tool_call",
			AgentContext: context,
			ToolCall:     tools.ToolCall{ID: "call", Type: "function", Function: tools.FunctionCall{Arguments: content}},
		}
	case "output":
		event = &runtime.ToolCallOutputEvent{Type: "tool_call_output", ToolCallID: "call", Output: content, AgentContext: context}
	}
	return SessionEventMsg{Event: event, OriginSessionID: "session", TurnID: "turn", Epoch: 1, Projection: &PresentationState{}}
}

func canonicalMergeTestContent(event runtime.Event) string {
	switch e := event.(type) {
	case *runtime.AgentChoiceEvent:
		return e.Content
	case *runtime.AgentChoiceReasoningEvent:
		return e.Content
	case *runtime.PartialToolCallEvent:
		return e.ToolCall.Function.Arguments
	case *runtime.ToolCallOutputEvent:
		return e.Output
	default:
		return ""
	}
}

// BenchmarkMergeEventsAgentChoice measures the cost of merging a typical
// throttle-window's worth of streaming chunks. The pre-fix implementation did
// `merged.Content + next.Content` repeatedly, which is O(N^2) in chunk count;
// the strings.Builder version is O(N).
func BenchmarkMergeEventsAgentChoice(b *testing.B) {
	for _, n := range []int{16, 64, 256} {
		b.Run("chunks="+strconv.Itoa(n), func(b *testing.B) {
			events := buildAgentChoiceEvents(n)
			a := &App{}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = a.mergeEvents(events)
			}
		})
	}
}

// BenchmarkMergeEventsPartialToolCall measures the same thing for tool-call
// argument deltas, which use a structurally similar concatenation pattern.
func BenchmarkMergeEventsPartialToolCall(b *testing.B) {
	for _, n := range []int{16, 64, 256} {
		b.Run("chunks="+strconv.Itoa(n), func(b *testing.B) {
			events := buildPartialToolCallEvents(n)
			a := &App{}

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = a.mergeEvents(events)
			}
		})
	}
}

func buildAgentChoiceEvents(n int) []any {
	const chunk = "the quick brown fox jumps over the lazy dog. "
	events := make([]any, 0, n)
	for range n {
		events = append(events, &runtime.AgentChoiceEvent{
			Content:      chunk,
			AgentContext: runtime.AgentContext{AgentName: "agent"},
		})
	}
	return events
}

func buildPartialToolCallEvents(n int) []any {
	const chunk = `,"key":"value"`
	events := make([]any, 0, n)
	for range n {
		events = append(events, &runtime.PartialToolCallEvent{
			ToolCall: tools.ToolCall{
				ID:       "call-1",
				Function: tools.FunctionCall{Arguments: chunk},
			},
		})
	}
	return events
}
