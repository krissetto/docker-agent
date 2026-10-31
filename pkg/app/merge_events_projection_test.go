package app

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	chatmessages "github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/streamcontent"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Each window is one throttle flush: app.mergeEvents runs over it before the
// chat page projects the result. The oracle projects the same events one by
// one without merging; the merged projection must render identically.
func TestMergeEventsProjectedRowsMatchUnmergedStream(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name    string
		windows [][]any
		// lines that must each appear on a single rendered line at width 80
		wantLines []string
	}{
		{
			name: "work before content",
			windows: [][]any{
				{toolCallEvent("t1")},
				{choiceEvent("m1", "Hello "), choiceEvent("m1", "streaming ")},
				{choiceEvent("m1", "world ")},
				{choiceEvent("m1", "again "), choiceEvent("m1", "and more")},
			},
			wantLines: []string{"Hello streaming world again and more"},
		},
		{
			name: "work after content",
			windows: [][]any{
				{choiceEvent("m1", "Hello "), choiceEvent("m1", "streaming ")},
				{choiceEvent("m1", "world ")},
				{toolCallEvent("t1")},
				{choiceEvent("m1", "again "), choiceEvent("m1", "and more")},
			},
			wantLines: []string{"Hello streaming world", "again and more"},
		},
		{
			name: "distinct runtime ids in one burst",
			windows: [][]any{
				{choiceEvent("m1", "first "), choiceEvent("m2", "second "), choiceEvent("m2", "third")},
				{choiceEvent("m2", " fourth")},
			},
			wantLines: []string{"first", "second third fourth"},
		},
		{
			name: "reasoning then content",
			windows: [][]any{
				{reasoningEvent("m1", "think "), reasoningEvent("m1", "more")},
				{reasoningEvent("m1", " again")},
				{choiceEvent("m1", "Answer "), choiceEvent("m1", "given")},
			},
			wantLines: []string{"Answer given"},
		},
	}

	for _, sc := range scenarios {
		for _, wrap := range []bool{false, true} {
			name := sc.name
			if wrap {
				name += "/session-wrapped"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				windows := sc.windows
				if wrap {
					windows = wrapWindows(windows)
				}
				a := &App{}
				merged := make([][]any, 0, len(windows))
				unmerged := make([][]any, 0)
				for _, w := range windows {
					merged = append(merged, a.mergeEvents(w))
					for _, ev := range w {
						unmerged = append(unmerged, []any{ev})
					}
				}

				for _, width := range []int{80, 30} {
					want := projectWindows(t, width, unmerged)
					got := projectWindows(t, width, merged)
					assert.Equal(t, want, got, "merged projection must match the unmerged stream at width %d", width)
				}

				frame := projectWindows(t, 80, merged)
				for _, line := range sc.wantLines {
					assert.Contains(t, frame, line, "expected one rendered line with %q at width 80", line)
				}
			})
		}
	}
}

func choiceEvent(messageID, content string) *runtime.AgentChoiceEvent {
	return &runtime.AgentChoiceEvent{
		Type:         "agent_choice",
		Content:      content,
		SessionID:    "session",
		MessageID:    messageID,
		AgentContext: runtime.AgentContext{AgentName: "root"},
	}
}

func reasoningEvent(messageID, content string) *runtime.AgentChoiceReasoningEvent {
	return &runtime.AgentChoiceReasoningEvent{
		Type:         "agent_choice_reasoning",
		Content:      content,
		SessionID:    "session",
		MessageID:    messageID,
		AgentContext: runtime.AgentContext{AgentName: "root"},
	}
}

func toolCallEvent(id string) *runtime.ToolCallEvent {
	return runtime.ToolCall(
		tools.ToolCall{ID: id, Type: "function", Function: tools.FunctionCall{Name: "read_file", Arguments: "{}"}},
		tools.Tool{Name: "read_file"},
		"root",
	).(*runtime.ToolCallEvent)
}

func wrapWindows(windows [][]any) [][]any {
	out := make([][]any, 0, len(windows))
	for _, w := range windows {
		wrapped := make([]any, 0, len(w))
		for _, ev := range w {
			wrapped = append(wrapped, SessionEventMsg{
				Event:           ev.(runtime.Event),
				OriginSessionID: "session",
				TurnID:          "turn",
				Epoch:           1,
				Projection:      &PresentationState{},
			})
		}
		out = append(out, wrapped)
	}
	return out
}

// projectWindows feeds each window through the same identity rules the chat page
// applies (streamcontent.Tracker, then the messages model) and returns the
// stripped frame. Only Work rows and assistant/reasoning content are projected.
func projectWindows(t *testing.T, width int, windows [][]any) string {
	t.Helper()

	m := chatmessages.NewScrollableView(animation.NewRuntime(), width, 40, &service.SessionState{})
	if compact, ok := m.(interface{ SetCompactWorkLog(enabled bool) tea.Cmd }); ok {
		compact.SetCompactWorkLog(true)
	}
	var ids streamcontent.Tracker
	for _, w := range windows {
		for _, item := range w {
			ev := item
			if wrapped, ok := item.(SessionEventMsg); ok {
				ev = wrapped.Event
			}
			switch e := ev.(type) {
			case *runtime.AgentChoiceEvent:
				id := ids.Resolve(e.SessionID, e.MessageID)
				m.AppendAssistantContent(id.SessionID, id.MessageID, e.AgentName, e.Content)
			case *runtime.AgentChoiceReasoningEvent:
				id := ids.Resolve(e.SessionID, e.MessageID)
				m.AppendReasoningContent(id.SessionID, id.MessageID, e.AgentName, e.Content)
			case *runtime.ToolCallEvent:
				m.AddOrUpdateToolCall(e.AgentName, e.ToolCall, e.ToolDefinition, types.ToolStatusCompleted)
			default:
				t.Fatalf("unexpected event %T", ev)
			}
		}
	}
	return ansi.Strip(m.View())
}
