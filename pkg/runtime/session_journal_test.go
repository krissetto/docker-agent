package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestUpdateTitlePersistsBeforeOrderedPublishAndSeedsLateAttach(t *testing.T) {
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	sess := session.New(session.WithID("title-intent"), session.WithAgentName("root"))
	require.NoError(t, store.AddSession(t.Context(), sess))
	rt := newPersistedSessionRuntime(t, store)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
	require.NoError(t, err)

	require.NoError(t, handle.UpdateTitle(t.Context(), "Canonical title"))
	loaded, err := store.GetSession(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "Canonical title", loaded.TitleSnapshot(), "publish follows durable mutation")

	zero := uint64(0)
	attached, err := handle.Observe(t.Context(), ObserveOptions{Since: &zero, SinceEpoch: sessionObservationEpoch(t, handle)})
	require.NoError(t, err)
	defer attached.Cancel()
	assert.Equal(t, "Canonical title", attached.Primary().Session.TitleSnapshot())
	assert.Nil(t, attached.SessionsAdded)
	require.Len(t, attached.Replay, 1)
	title, ok := attached.Replay[0].Event.(*SessionTitleEvent)
	require.True(t, ok)
	assert.Equal(t, "Canonical title", title.Title)
	assert.Equal(t, uint64(1), attached.Replay[0].Sequence)
}

func TestConcurrentAttachDetachDoesNotAffectSessionLifetime(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	first, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	second, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)

	first.Cancel()
	for range first.Events {
	}
	submission, err := handle.Submit(t.Context(), TurnInput{Content: "still owned"})
	require.NoError(t, err)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case envelope := <-second.Events:
			if envelope.TurnID == submission.TurnID {
				if _, settled := envelope.Event.(*StreamStoppedEvent); settled {
					second.Cancel()
					return
				}
			}
		case <-deadline:
			t.Fatal("detaching one client affected the session or its other client")
		}
	}
}

func TestInteractionJournalCarriesImmutableCorrelation(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver
	zero := uint64(0)
	observation, err := handle.Observe(t.Context(), ObserveOptions{Since: &zero, SinceEpoch: sessionObservationEpoch(t, handle)})
	require.NoError(t, err)
	defer observation.Cancel()

	events := []Event{
		&ToolCallConfirmationEvent{Type: "tool_call_confirmation", SessionID: sess.ID, RequestID: "confirm-1"},
		MaxIterationsReachedForSession(3, sess.ID, "max-1"),
		&ElicitationRequestEvent{Type: "elicitation_request", SessionID: sess.ID, RequestID: "elicit-1", ElicitationID: "e-1"},
	}
	for _, event := range events {
		driver.RegisterInteraction(interactionEventID(event), interactionKindOf(event), event)
		driver.events.Publish(sess.ID, event)
		envelope := <-observation.Events
		assert.Equal(t, interactionEventID(event), envelope.InteractionID)
		assert.Same(t, event, envelope.Event)
	}

	replayed, err := handle.Observe(t.Context(), ObserveOptions{Since: &zero, SinceEpoch: sessionObservationEpoch(t, handle)})
	require.NoError(t, err)
	defer replayed.Cancel()
	require.Len(t, replayed.Replay, len(events))
	for i, event := range events {
		assert.Equal(t, interactionEventID(event), replayed.Replay[i].InteractionID)
	}
}

func interactionKindOf(event Event) InteractionKind {
	switch event.(type) {
	case *ToolCallConfirmationEvent:
		return InteractionConfirmation
	case *MaxIterationsReachedEvent:
		return InteractionMaxIterations
	default:
		return InteractionElicitation
	}
}

func TestInteractionSnapshotIsImmutableAndRejectsStaleResponse(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver
	event := ElicitationRequest("choose", "form", nil, "", "elicitation", "", sess.ID, nil, "root").(*ElicitationRequestEvent)
	event.RequestID = "turn-request"
	driver.RegisterInteraction("turn-request", InteractionElicitation, event)

	attached, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer attached.Cancel()
	require.Len(t, attached.Primary().Interactions, 1)
	item := attached.Primary().Interactions[0]
	assert.Equal(t, sess.ID, item.SessionID)
	assert.Equal(t, "turn-request", item.InteractionID)
	assert.Equal(t, "elicitation", item.ElicitationID)
	assert.Equal(t, InteractionElicitation, item.Kind)

	driver.mu.Lock()
	delete(driver.interactions, "turn-request")
	driver.mu.Unlock()
	err = handle.Respond(t.Context(), InteractionResponse{InteractionID: item.InteractionID, Kind: item.Kind, ElicitationID: item.ElicitationID})
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorStale, sessionErr.Kind)
	assert.Equal(t, "turn-request", attached.Primary().Interactions[0].InteractionID, "snapshot remains immutable")
}
