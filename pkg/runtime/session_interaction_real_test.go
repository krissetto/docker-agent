package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestLocalRuntimeGeneratedToolConfirmationObserveRespond(t *testing.T) {
	rt, sess, executed, agentTools := makeJudgedRuntime(t, "ask", "approval required", nil)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	driver := handle.(*sessionHandle).driver
	sink := EventSinkFunc(func(event Event) { driver.events.Publish(sess.ID, event) })
	done := make(chan struct{})
	go func() {
		rt.processToolCalls(t.Context(), sess, []tools.ToolCall{{ID: "real-confirm", Function: tools.FunctionCall{Name: "the_tool", Arguments: `{}`}}}, agentTools, sink)
		close(done)
	}()
	for envelope := range observation.Events {
		if _, ok := envelope.Event.(*ToolCallConfirmationEvent); !ok {
			continue
		}
		require.NotEmpty(t, envelope.InteractionID)
		require.NoError(t, handle.Respond(t.Context(), InteractionResponse{InteractionID: envelope.InteractionID, Kind: InteractionConfirmation, Resume: ResumeApprove()}))
		break
	}
	<-done
	assert.True(t, *executed)
}

func TestLocalRuntimeGeneratedMaxIterationsObserveRespondContinueAndReject(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response ResumeRequest
		want     iterationDecision
	}{
		{"continue", ResumeApprove(), iterationContinue},
		{"reject", ResumeReject("stop"), iterationStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, sess := newSessionFixture(t)
			handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			driver := handle.(*sessionHandle).driver
			driver.mu.Lock()
			driver.running, driver.activeRequestID = true, "turn-max"
			driver.generation++
			driver.mu.Unlock()
			observation, err := handle.Observe(t.Context(), ObserveOptions{})
			require.NoError(t, err)
			defer observation.Cancel()
			sink := EventSinkFunc(func(event Event) { driver.events.Publish(sess.ID, event) })
			result := make(chan iterationDecision, 1)
			go func() {
				_, decision := rt.enforceMaxIterations(t.Context(), sess, rt.currentAgent(), 1, 1, sink)
				result <- decision
			}()
			for envelope := range observation.Events {
				if _, ok := envelope.Event.(*MaxIterationsReachedEvent); !ok {
					continue
				}
				require.NotEmpty(t, envelope.InteractionID)
				require.NoError(t, handle.Respond(t.Context(), InteractionResponse{InteractionID: envelope.InteractionID, Kind: InteractionMaxIterations, Resume: tc.response}))
				break
			}
			assert.Equal(t, tc.want, <-result)
		})
	}
}

func TestLocalRuntimeGeneratedConcurrentElicitationsObserveRespondUnique(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver
	driver.mu.Lock()
	driver.running, driver.activeRequestID = true, "same-turn"
	driver.generation++
	driver.mu.Unlock()
	observation, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	ids := []string{"elicitation-one", "elicitation-two"}
	for _, id := range ids {
		event := ElicitationRequest("question", "form", nil, "", id, "server-"+id, sess.ID, nil, "root").(*ElicitationRequestEvent)
		event.RequestID = id
		driver.RegisterInteraction(id, InteractionElicitation, event)
		driver.events.Publish(sess.ID, event)
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		envelope := <-observation.Events
		event, ok := envelope.Event.(*ElicitationRequestEvent)
		if !ok {
			continue
		}
		require.NotEmpty(t, envelope.InteractionID)
		seen[envelope.InteractionID] = true
		waiter := rt.elicitationWaiters.register(event.ElicitationID)

		require.NoError(t, handle.Respond(t.Context(), InteractionResponse{InteractionID: envelope.InteractionID, Kind: InteractionElicitation, ElicitationID: event.ElicitationID, Elicitation: ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"id": event.ElicitationID}}}))
		assert.Equal(t, event.ElicitationID, (<-waiter.ch).Content["id"])
		rt.elicitationWaiters.abandon(event.ElicitationID, waiter)
	}
	assert.Len(t, seen, 2)
}
