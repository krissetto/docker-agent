package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestRunEntrypointsDeliverOneElicitationAcrossObservationStyles(t *testing.T) {
	entrypoints := []struct {
		name      string
		run       func(*App)
		wantRetry bool
	}{
		{name: "run", run: func(a *App) { a.Run(t.Context(), func() {}, "hello", nil) }},
		{name: "retry", run: func(a *App) { a.Retry(t.Context(), func() {}) }, wantRetry: true},
		{name: "run-with-message", run: func(a *App) { a.RunWithMessage(t.Context(), func() {}, session.UserMessage("hello")) }},
	}
	for _, style := range []string{"local-live", "remote-replay"} {
		for _, entrypoint := range entrypoints {
			t.Run(style+"/"+entrypoint.name, func(t *testing.T) {
				sess := session.New(session.WithID(style + entrypoint.name))
				event := runtime.ElicitationRequest("question", "", nil, "", "elicitation", "", sess.ID, nil, "root")
				handle := &projectionSession{id: sess.ID, events: make(chan runtime.SessionEvent, 8), errors: make(chan error)}
				observed := make(chan struct{})
				handle.observe = func(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
					close(observed)
					observation := runtime.Observation{
						Initial: []runtime.SessionSnapshot{{Status: runtime.SessionStatus{SessionID: sess.ID}}},
						Events:  handle.events, Cancel: func() {},
					}
					if style == "remote-replay" {
						observation.Replay = []runtime.SessionEvent{{Sequence: 1, TranscriptPosition: -1, InteractionID: "interaction", Event: event}}
					}
					return observation, nil
				}
				a := New(t.Context(), &projectionSessions{session: handle}, sess, runtime.SessionBinding{}, WithRuntimeServices(&mockRuntime{}))
				require.True(t, a.startSessionEventBridge(t.Context()))
				<-observed
				entrypoint.run(a)

				handle.mu.Lock()
				require.Len(t, handle.inputs, 1)
				assert.Equal(t, entrypoint.wantRetry, handle.inputs[0].Retry)
				handle.mu.Unlock()

				if style == "local-live" {
					handle.events <- runtime.SessionEvent{TranscriptPosition: -1, InteractionID: "interaction", Event: event}
				}
				seenElicitation := 0
				for {
					got := (<-a.events).(SessionEventMsg)
					if _, ok := got.Event.(*runtime.ElicitationRequestEvent); ok {
						seenElicitation++
						break
					}
				}
				// A second lifecycle event is an ordering fence: if the elicitation
				// were duplicated, it would necessarily appear before this marker.
				marker := runtime.Warning("fence", "")
				handle.events <- runtime.SessionEvent{Sequence: 2, TranscriptPosition: -1, Event: marker}
				for {
					got := (<-a.events).(SessionEventMsg)
					if _, duplicate := got.Event.(*runtime.ElicitationRequestEvent); duplicate {
						seenElicitation++
					}
					if got.Event == marker {
						break
					}
				}
				assert.Equal(t, 1, seenElicitation)
			})
		}
	}
}
