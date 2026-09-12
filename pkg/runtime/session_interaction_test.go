package runtime

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func respondToElicitation(t *testing.T, rt *LocalRuntime, event *ElicitationRequestEvent, result ElicitationResult) {
	t.Helper()
	sessionID := event.SessionID
	if sessionID == "" {
		sessionID = "elicitation-test-session"
	}
	handle, err := rt.SessionByID(sessionID)
	if err != nil {
		handle, err = rt.CreateSession(t.Context(), session.New(session.WithID(sessionID)), SessionBinding{AgentName: rt.CurrentAgentName(t.Context())})
		require.NoError(t, err)
	}
	requestID := event.RequestID
	if requestID == "" {
		requestID = "elicitation-test-request-" + event.ElicitationID
	}
	driver, ok := rt.sessionDrivers.Lookup(sessionID)
	require.True(t, ok)
	driver.RegisterInteraction(requestID, InteractionElicitation)
	require.NoError(t, handle.Respond(t.Context(), InteractionResponse{
		InteractionID: requestID,
		Kind:          InteractionElicitation,
		ElicitationID: event.ElicitationID,
		Elicitation:   result,
	}))
}
