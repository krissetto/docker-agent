package types

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func TestPresentationHiddenPreservesIndependentSurfaces(t *testing.T) {
	for _, text := range []string{"", " ", "\t", "\n", "\r\n\t", "\u00a0"} {
		require.True(t, Agent(MessageTypeAssistant, "root", text).PresentationHidden())
		require.Equal(t, text, Agent(MessageTypeAssistant, "root", text).Content)
		input := session.UserMessage(text)
		input.InputOrigin = session.InputOriginAgent
		require.True(t, Input(input).PresentationHidden())
		for _, origin := range []session.InputOrigin{"", session.InputOriginUser, session.InputOriginRuntime, "future-origin"} {
			input.InputOrigin = origin
			require.False(t, Input(input).PresentationHidden())
		}
	}
	for _, msg := range []*Message{
		{Type: MessageTypeAgentInput, InputOrigin: session.InputOriginAgent, Attachments: []UserAttachment{{}}},
		{Type: MessageTypeAgentInput, InputOrigin: session.InputOriginAgent, Pending: true},
		{Type: MessageTypeAgentInput, InputOrigin: session.InputOriginAgent, ReportOutcome: session.ReportOutcomeFinished},
		{Type: MessageTypeAgentInput, InputOrigin: session.InputOriginAgent, ReceivedBody: "finished its turn"},
		{Type: MessageTypeAssistant, AssistantMedia: []AssistantMedia{{Fallback: "image"}}},
		{Type: MessageTypeAssistant, Content: "\tindented prose"},
		{Type: MessageTypeAssistantReasoningBlock}, {Type: MessageTypeToolCall}, {Type: MessageTypeRuntimeNotice},
	} {
		require.False(t, msg.PresentationHidden())
	}
}
