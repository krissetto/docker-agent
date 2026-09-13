package app

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestNewAppDefaultThrottleStreamsCanonicalBurst(t *testing.T) {
	for _, kind := range []string{"text", "reasoning", "arguments", "output"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				a := New(ctx, nil, nil, runtime.SessionBinding{})
				require.Equal(t, 50*time.Millisecond, a.throttleDuration)
				a.bridgeEpoch.Store(1)
				head := &PresentationState{}
				a.presentation.Store(head)

				const chunks = 1500
				got := make(chan any, chunks+3)
				ready, done := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(done)
					a.Subscribe(ctx, func(msg any) { got <- msg }, SubscribeOptions{PreserveSessionMetadata: true, Ready: ready})
				}()
				defer func() {
					cancel()
					a.Close()
					<-done
					synctest.Wait()
				}()
				<-ready

				original := defaultThrottleEvent(kind, "word ")
				for range chunks {
					require.True(t, a.sendBridgedEventFrom(ctx, "turn", original, false, "session", 1))
				}
				synctest.Wait()
				require.Empty(t, got, "a provider burst must not flood the subscriber")

				// Virtual time proves live delivery uses the constructor's window, not a stop event.
				<-time.After(49 * time.Millisecond)
				synctest.Wait()
				require.Empty(t, got)
				<-time.After(time.Millisecond)
				synctest.Wait()
				require.Len(t, got, 1)
				live := (<-got).(SessionEventMsg)
				require.Equal(t, defaultThrottleEvent(kind, strings.Repeat("word ", chunks)), live.Event)
				require.Equal(t, "session", live.OriginSessionID)
				require.Equal(t, "turn", live.TurnID)
				require.Equal(t, uint64(1), live.Epoch)
				require.False(t, live.Seed)
				require.Same(t, head, live.Projection)
				require.Equal(t, defaultThrottleEvent(kind, "word "), original, "merging must not mutate provider events")
				require.NotSame(t, original, live.Event)

				beforeStop := time.Now()
				for range 2 {
					require.True(t, a.sendBridgedEventFrom(ctx, "turn", original, false, "session", 1))
				}
				stop := runtime.StreamStopped("session", "agent", "done")
				require.True(t, a.sendBridgedEventFrom(ctx, "turn", stop, false, "session", 1))
				synctest.Wait()
				require.Equal(t, beforeStop, time.Now(), "terminal boundaries flush without waiting for the window")
				require.Len(t, got, 2)
				tail := (<-got).(SessionEventMsg)
				require.Equal(t, "word word ", defaultThrottleContent(tail.Event))
				require.Same(t, head, tail.Projection)
				terminal := (<-got).(SessionEventMsg)
				require.Same(t, stop, terminal.Event)
				require.Same(t, a.Presentation(), terminal.Projection)
				require.Equal(t, "word ", defaultThrottleContent(original))
			})
		})
	}
}

func defaultThrottleEvent(kind, content string) runtime.Event {
	agent := runtime.AgentContext{AgentName: "agent"}
	switch kind {
	case "text":
		return &runtime.AgentChoiceEvent{Type: "agent_choice", SessionID: "session", Content: content, AgentContext: agent}
	case "reasoning":
		return &runtime.AgentChoiceReasoningEvent{Type: "agent_choice_reasoning", SessionID: "session", Content: content, AgentContext: agent}
	case "arguments":
		return &runtime.PartialToolCallEvent{
			Type: "partial_tool_call", AgentContext: agent,
			ToolCall: tools.ToolCall{ID: "call", Type: "function", Function: tools.FunctionCall{Name: "check", Arguments: content}},
		}
	default:
		return &runtime.ToolCallOutputEvent{Type: "tool_call_output", ToolCallID: "call", Output: content, AgentContext: agent}
	}
}

func defaultThrottleContent(event runtime.Event) string {
	switch event := event.(type) {
	case *runtime.AgentChoiceEvent:
		return event.Content
	case *runtime.AgentChoiceReasoningEvent:
		return event.Content
	case *runtime.PartialToolCallEvent:
		return event.ToolCall.Function.Arguments
	case *runtime.ToolCallOutputEvent:
		return event.Output
	default:
		return ""
	}
}
