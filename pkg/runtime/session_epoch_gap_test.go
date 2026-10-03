package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSessionEventHubRestartAheadCursorRequiresFreshSnapshot(t *testing.T) {
	// A new hub is the journal epoch after process restart, before any publish.
	hub := newSessionEventHubWithLimits(4, 4096)
	since := uint64(99)
	replay, _, cancel, cursor := hub.SubscribeSequenced("s", &since, 1)
	cancel()
	require.Len(t, replay, 1)
	assert.True(t, replay[0].Gap)
	assert.Equal(t, uint64(0), cursor)
	_, events, cancel, cursor := hub.SubscribeSequenced("s", nil, 1)
	defer cancel()
	assert.Zero(t, cursor)
	hub.Publish("s", AgentChoice("root", "s", "after restart"))
	assert.Equal(t, uint64(1), (<-events).Sequence)
}

func TestSessionObservationRestartCursorCollisionUsesFreshBaseline(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(strconv.FormatBool(remote), func(t *testing.T) {
			rt := newPersistedSessionRuntime(t, session.NewInMemorySessionStore())
			sess := session.New(session.WithID("restart"))
			sess.AddMessage(session.UserMessage("fresh committed transcript"))
			handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			driver := handle.(*sessionHandle).driver
			event := &ToolCallConfirmationEvent{Type: "tool_call_confirmation", SessionID: sess.ID, RequestID: "fresh-approval"}
			driver.RegisterInteraction("fresh-approval", InteractionConfirmation, event)
			for range 10 {
				driver.events.Publish(sess.ID, AgentChoice("root", sess.ID, "fresh tail"))
			}
			if remote {
				localHandle := handle
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "5", r.URL.Query().Get("since"))
					assert.Equal(t, "previous-process", r.URL.Query().Get("since_epoch"))
					cursor, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
					observation, observeErr := localHandle.Observe(r.Context(), ObserveOptions{Since: &cursor, SinceEpoch: r.URL.Query().Get("since_epoch")})
					if !assert.NoError(t, observeErr) {
						return
					}
					defer observation.Cancel()
					primary := observation.Primary()
					snapshot := remoteSessionSnapshot{Session: primary.Session, Status: api.SessionStatus[SessionState](primary.Status), Cursor: primary.Cursor, Epoch: primary.Epoch, TranscriptPosition: primary.TranscriptPosition}
					for _, interaction := range primary.Interactions {
						raw, _ := json.Marshal(interaction.Event)
						snapshot.Interactions = append(snapshot.Interactions, api.SessionInteraction[InteractionKind, json.RawMessage]{SessionID: interaction.SessionID, InteractionID: interaction.InteractionID, Kind: interaction.Kind, Event: raw})
					}
					write := func(message remoteSessionStreamMessage) {
						data, _ := json.Marshal(message)
						fmt.Fprintf(w, "data: %s\n\n", data)
					}
					write(remoteSessionStreamMessage{Version: sessionWireVersion, Type: "snapshot", Snapshot: &snapshot})
					for _, envelope := range observation.Replay {
						raw, _ := json.Marshal(envelope.Event)
						wire := remoteSessionEnvelope{Version: sessionWireVersion, SessionID: envelope.SessionID, Epoch: envelope.Epoch, Sequence: envelope.Sequence, TranscriptPosition: envelope.TranscriptPosition, Event: raw}
						write(remoteSessionStreamMessage{Version: sessionWireVersion, Type: "event", Envelope: &wire})
					}
					write(remoteSessionStreamMessage{Version: sessionWireVersion, Type: "ready", Cursor: primary.Cursor})
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer server.Close()
				client, clientErr := NewClient(server.URL)
				require.NoError(t, clientErr)
				transport, transportErr := NewSessionTransport(client)
				require.NoError(t, transportErr)
				handle, err = transport.SessionByID(sess.ID)
				require.NoError(t, err)
			}
			cursor := uint64(5)
			observation, err := handle.Observe(t.Context(), ObserveOptions{Since: &cursor, SinceEpoch: "previous-process"})
			require.NoError(t, err)
			defer observation.Cancel()
			primary := observation.Primary()
			assert.Equal(t, driver.events.epoch, primary.Epoch)
			assert.Equal(t, uint64(10), primary.Cursor)
			assert.Equal(t, "fresh committed transcript", primary.Session.Messages[0].Message.Message.Content)
			require.Len(t, primary.Interactions, 1)
			assert.Equal(t, "fresh-approval", primary.Interactions[0].InteractionID)
			require.NotEmpty(t, observation.Replay)
			for _, seed := range observation.Replay {
				assert.Zero(t, seed.Sequence)
				assert.Equal(t, primary.Epoch, seed.Epoch)
			}
		})
	}
}
