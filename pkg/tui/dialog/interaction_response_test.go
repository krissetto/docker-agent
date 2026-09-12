package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestDialogsProduceCompleteInteractionResponses(t *testing.T) {
	ref := ElicitationRef{SessionID: "session", RequestID: "interaction", ElicitationID: "elicitation"}
	confirmation := NewToolConfirmationDialog(animation.NewRuntime(), &runtime.ToolCallConfirmationEvent{SessionID: "session", RequestID: "interaction"}, &service.SessionState{}).(*toolConfirmationDialog)
	form := NewElicitationDialog("question", nil, nil, ref).(*ElicitationDialog)
	form.responseInput.SetValue("answer")
	elicitation := func(content map[string]any) messages.InteractionResponseMsg {
		return messages.InteractionResponseMsg{SessionID: "session", Response: runtime.InteractionResponse{
			InteractionID: "interaction", Kind: runtime.InteractionElicitation, ElicitationID: "elicitation",
			Elicitation: runtime.ElicitationResult{Action: tools.ElicitationActionAccept, Content: content},
		}}
	}
	tests := []struct {
		name string
		cmd  func() tea.Cmd
		want messages.InteractionResponseMsg
	}{
		{name: "confirmation", cmd: func() tea.Cmd { return messageCommand(confirmation.response(runtime.ResumeApprove())) }, want: messages.InteractionResponseMsg{SessionID: "session", Response: runtime.InteractionResponse{InteractionID: "interaction", Kind: runtime.InteractionConfirmation, Resume: runtime.ResumeApprove()}}},
		{name: "max", cmd: func() tea.Cmd {
			_, cmd := NewMaxIterationsDialog(10, "session", "interaction").Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
			return cmd
		}, want: messages.InteractionResponseMsg{SessionID: "session", Response: runtime.InteractionResponse{InteractionID: "interaction", Kind: runtime.InteractionMaxIterations, Resume: runtime.ResumeApprove()}}},
		{name: "form", cmd: func() tea.Cmd { _, cmd := form.submit(); return cmd }, want: elicitation(map[string]any{"response": "answer"})},
		{name: "url", cmd: func() tea.Cmd {
			return NewURLElicitationDialog(t.Context(), "question", "", ref).(*URLElicitationDialog).respond(tools.ElicitationActionAccept)
		}, want: elicitation(nil)},
		{name: "oauth", cmd: func() tea.Cmd {
			_, cmd := NewOAuthAuthorizationDialog("server", ref).(*oauthAuthorizationDialog).respond(tools.ElicitationActionAccept)
			return cmd
		}, want: elicitation(nil)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got messages.InteractionResponseMsg
			for _, msg := range collectMsgs(test.cmd()) {
				if response, ok := msg.(messages.InteractionResponseMsg); ok {
					got = response
				}
			}
			require.NotEmpty(t, got.SessionID)
			assert.Equal(t, test.want, got)
		})
	}
}

func messageCommand(msg messages.InteractionResponseMsg) tea.Cmd {
	return func() tea.Msg { return msg }
}
