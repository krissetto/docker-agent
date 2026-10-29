package app

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestProjectionResetRetainsAndClearsBackgroundProcesses(t *testing.T) {
	a := newMetadataTestApp(t)
	got := make(chan any, 4)
	ready := make(chan struct{})
	go a.Subscribe(t.Context(), func(msg any) { got <- msg }, SubscribeOptions{PreserveSessionMetadata: true, Ready: ready})
	<-ready
	sink := &appProjectionSink{app: a, ctx: t.Context(), sessionID: "owner"}
	process := tools.BackgroundProcess{ID: "adopted", SessionID: "owner", ToolCallID: "shell-call", State: tools.BackgroundProcessRunning}
	sink.Reset(runtime.SessionSnapshot{Session: session.New(session.WithID("owner")), Status: runtime.SessionStatus{SessionID: "owner"}, BackgroundProcesses: []tools.BackgroundProcess{process}})
	first := (<-got).(SessionEventMsg).Event.(*SessionResetEvent)
	require.Equal(t, []tools.BackgroundProcess{process}, first.Snapshot.BackgroundProcesses)

	// A nil list is an authoritative empty replacement, even while transcript
	// identity and session status are unchanged.
	sink.Reset(runtime.SessionSnapshot{Session: session.New(session.WithID("owner")), Status: runtime.SessionStatus{SessionID: "owner"}})
	second := (<-got).(SessionEventMsg).Event.(*SessionResetEvent)
	require.Nil(t, second.Snapshot.BackgroundProcesses)
}
