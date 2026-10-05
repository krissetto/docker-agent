package runtime

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
)

func respondToElicitation(t *testing.T, rt *LocalRuntime, event *ElicitationRequestEvent, result ElicitationResult) {
	t.Helper()
	sessionID := event.SessionID
	if sessionID == "" {
		sessionID = t.Name() + "/elicitation-test-session"
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
	driver.mu.Lock()
	_, registered := driver.interactions[requestID]
	driver.mu.Unlock()
	if !registered {
		driver.RegisterInteraction(requestID, InteractionElicitation, event)
	}
	require.NoError(t, handle.Respond(t.Context(), InteractionResponse{
		InteractionID: requestID,
		Kind:          InteractionElicitation,
		ElicitationID: event.ElicitationID,
		Elicitation:   result,
	}))
}

func respondResumeForTest(t *testing.T, rt *LocalRuntime, sessionID string, response ResumeRequest) {
	t.Helper()
	require.NoError(t, respondResume(rt, sessionID, response))
}

func respondResume(rt *LocalRuntime, sessionID string, response ResumeRequest) error {
	var driver *sessionDriver
	var id string
	deadline := time.After(time.Second)
	for id == "" {
		var ok bool
		driver, ok = rt.sessionDrivers.Lookup(sessionID)
		if ok {
			driver.mu.Lock()
			for request, interaction := range driver.interactions {
				if interaction.resume != nil {
					id = request
					break
				}
			}
			driver.mu.Unlock()
		}
		if id != "" {
			break
		}
		select {
		case <-deadline:
			return fmt.Errorf("no scoped interaction for session %s", sessionID)
		case <-time.After(time.Millisecond):
		}
	}

	return driver.Respond(InteractionResponse{InteractionID: id, Kind: func() InteractionKind {
		driver.mu.Lock()
		defer driver.mu.Unlock()
		return driver.interactions[id].kind
	}(), Resume: response})
}
