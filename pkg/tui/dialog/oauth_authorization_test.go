package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestOAuthAuthorizationProducesCompleteResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    rune
		action tools.ElicitationAction
	}{
		{name: "allow", key: 'y', action: tools.ElicitationActionAccept},
		{name: "deny", key: 'n', action: tools.ElicitationActionDecline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewOAuthAuthorizationDialog("https://mcp.example", ElicitationRef{SessionID: "target", RequestID: "request-7", ElicitationID: "elicitation-9"})
			_, cmd := d.Update(tea.KeyPressMsg{Code: tc.key, Text: string(tc.key)})
			require.NotNil(t, cmd)
			var response messages.InteractionResponseMsg
			for _, msg := range collectMsgs(cmd) {
				if value, ok := msg.(messages.InteractionResponseMsg); ok {
					response = value
				}
			}
			assert.Equal(t, "target", response.SessionID)
			assert.Equal(t, runtime.InteractionResponse{
				InteractionID: "request-7", Kind: runtime.InteractionElicitation,
				ElicitationID: "elicitation-9", Elicitation: runtime.ElicitationResult{Action: tc.action},
			}, response.Response)
		})
	}
}
