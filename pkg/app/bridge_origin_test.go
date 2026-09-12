package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestBridgeOriginAllowsChildPayloadAndRejectsStaleRoot(t *testing.T) {
	ctx := t.Context()
	a := &App{ctx: func() context.Context { return ctx }, currentState: sessionState{session: session.New(session.WithID("root"))}, events: make(chan any, 8)}
	a.bridgeEpoch.Store(4)
	got := make(chan any, 8)
	ready := make(chan struct{})
	go a.Subscribe(ctx, func(msg any) { got <- msg }, SubscribeOptions{PreserveSessionMetadata: true, Ready: ready})
	<-ready

	child := runtime.StreamStarted("child", "worker")
	require.True(t, a.sendBridgedEventFrom(ctx, "", child, false, "root", 4))
	msg := (<-got).(SessionEventMsg)
	require.Equal(t, "root", msg.OriginSessionID)
	require.Equal(t, "child", msg.Event.(*runtime.StreamStartedEvent).SessionID)

	a.bridgeEpoch.Store(5)
	a.events <- SessionEventMsg{Event: runtime.StreamStopped("root", "root", "normal"), OriginSessionID: "root", Epoch: 4}
	select {
	case <-got:
		t.Fatal("stale root event crossed bridge epoch")
	default:
	}
}
