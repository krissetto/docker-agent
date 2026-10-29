package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestCapturePresentationMatchesReferenceForStreamedText(t *testing.T) {
	var text chat.AssistantTextAppender
	var got, want []chat.AssistantPart
	events := []Event{
		AgentChoice("root", "s", "hel"),
		AgentChoice("root", "s", "lo"),
		AgentChoiceReasoning("root", "s", "why"),
		AgentChoiceReasoning("root", "s", "more"),
		ToolCall(tools.ToolCall{ID: "c1", Function: tools.FunctionCall{Name: "read", Arguments: "{}"}}, tools.Tool{Name: "read"}, "root"),
		AgentChoice("root", "s", "after"),
		AgentChoice("root", "s", "!"),
		AgentChoiceReasoning("root", "s", "again"),
	}
	for _, ev := range events {
		got = capturePresentation(&text, got, ev)
		want = captureAssistantPresentation(want, ev)
	}
	require.Equal(t, want, got)
}

func TestHandleStreamSplitToolCallTagKeepsGatedTextOutOfPresentation(t *testing.T) {
	stream := newStreamBuilder().
		AddReasoning("think").
		AddContent("before ").
		AddContent(`<tool_call>{"name":"read","arguments":{}}</tool_call>`).
		AddContent(" tail").
		Build()
	var text chat.AssistantTextAppender
	var emitted []chat.AssistantPart
	res, err := handleStream(t.Context(), nil, stream, agent.New("root", "test"), nil, session.New(), nil, defaultTelemetry{}, EventSinkFunc(func(ev Event) { emitted = capturePresentation(&text, emitted, ev) }), time.Second)
	require.NoError(t, err)
	require.Len(t, res.Calls, 1)
	want := []chat.AssistantPart{
		{Type: chat.AssistantPartReasoning, Text: "think"},
		{Type: chat.AssistantPartContent, Text: "before "},
		{Type: chat.AssistantPartToolCall, ToolCallID: res.Calls[0].ID},
	}
	assert.Equal(t, want, res.Presentation)
	for i := range emitted {
		emitted[i].Tool = nil
	}
	assert.Equal(t, res.Presentation, emitted)
}
