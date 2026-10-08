package server

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSteeringRemoteCanonicalTransitions(t *testing.T) {
	sess := session.New(session.WithID("recipient"))
	accepted := runtime.PendingUserMessageAccepted(sess.ID, "incoming", "clean payload", nil, 3).(*runtime.PendingUserMessageAcceptedEvent)
	accepted.Timestamp = time.Time{}
	accepted.InputOrigin, accepted.InputMode, accepted.SenderID, accepted.SenderName = session.InputOriginAgent, "steer", "sender", "worker"
	promoted := runtime.PendingUserMessagePromoted(sess.ID, "incoming", accepted.Message, nil, 3).(*runtime.PendingUserMessagePromotedEvent)
	promoted.Timestamp = time.Time{}
	promoted.InputOrigin, promoted.InputMode, promoted.SenderID, promoted.SenderName = accepted.InputOrigin, accepted.InputMode, accepted.SenderID, accepted.SenderName
	canceled := runtime.PendingUserMessageCanceled(sess.ID, "withdrawn", 4).(*runtime.PendingUserMessageCanceledEvent)
	canceled.Timestamp = time.Time{}
	live := make(chan runtime.SessionEvent, 3)
	for i, event := range []runtime.Event{accepted, promoted, canceled} {
		live <- runtime.SessionEvent{SessionID: sess.ID, TurnID: "incoming", Epoch: "epoch", Sequence: uint64(i + 1), TranscriptPosition: 3, Event: event}
	}
	close(live)
	handle := &httpSession{id: sess.ID, agent: "root", attach: runtime.Observation{Initial: []runtime.SessionSnapshot{{Epoch: "epoch", Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID}, PendingInputs: []runtime.PendingInput{{TurnID: "incoming", Content: accepted.Message, InputOrigin: accepted.InputOrigin, InputMode: accepted.InputMode, SenderID: accepted.SenderID, SenderName: accepted.SenderName, SessionPosition: accepted.SessionPosition}}}}, Events: live, Cancel: func() {}}}
	server, _ := newSessionHTTPServer(t, &httpSessionRegistry{sessions: map[string]*httpSession{sess.ID: handle}})
	httpServer := httptest.NewServer(server.e)
	defer httpServer.Close()
	client, err := runtime.NewClient(httpServer.URL)
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID(sess.ID)
	require.NoError(t, err)
	observation, err := remote.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	require.Len(t, observation.Initial, 1)
	require.Equal(t, handle.attach.Initial[0].PendingInputs, observation.Initial[0].PendingInputs)
	for _, want := range []runtime.Event{accepted, promoted, canceled} {
		select {
		case got, ok := <-observation.Events:
			require.True(t, ok)
			require.Equal(t, "incoming", got.TurnID)
			require.Equal(t, sess.ID, got.SessionID)
			require.Equal(t, 3, got.TranscriptPosition)
			require.Equal(t, want, got.Event)
		case <-time.After(3 * time.Second):
			t.Fatal("missing remote transition")
		}
	}
}
