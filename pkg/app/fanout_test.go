package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func registerFanoutTestSubscriber(a *App, ctx context.Context, ch chan any) {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	if a.subscriberDone == nil {
		a.subscriberDone = make(map[chan any]<-chan struct{})
	}
	a.subs = append(a.subs, ch)
	a.subscriberDone[ch] = ctx.Done()
}

func TestFanOutPreservesEveryEventOnOverflow(t *testing.T) {
	a := &App{ctx: func() context.Context { return t.Context() }, events: make(chan any, 16)}
	ch := make(chan any, 1)
	registerFanoutTestSubscriber(a, t.Context(), ch)
	a.startFanOut()
	events := []any{runtime.AgentChoice("root", "s", "first"), runtime.PendingUserMessageCanceled("s", "turn", 0), &SessionResetEvent{}, runtime.StreamStopped("s", "root", "normal")}
	for _, event := range events {
		a.events <- event
	}
	for _, want := range events {
		select {
		case got := <-ch:
			assert.Equal(t, want, got)
		case <-time.After(time.Second):
			t.Fatal("fanout lost an event")
		}
	}
}

func TestFanOutCanceledSubscriberDoesNotBlockOthers(t *testing.T) {
	a := &App{ctx: func() context.Context { return t.Context() }, events: make(chan any, 16)}
	ctx, cancel := context.WithCancel(t.Context())
	slow, witness := make(chan any, 1), make(chan any, 16)
	registerFanoutTestSubscriber(a, ctx, slow)
	registerFanoutTestSubscriber(a, t.Context(), witness)
	a.startFanOut()
	first := runtime.StreamStarted("s", "root")
	a.events <- first
	require.Eventually(t, func() bool { return len(slow) == 1 }, time.Second, time.Millisecond)
	last := runtime.StreamStopped("s", "root", "normal")
	a.events <- last
	cancel()
	assert.Equal(t, first, <-witness)
	select {
	case got := <-witness:
		assert.Equal(t, last, got)
	case <-time.After(time.Second):
		t.Fatal("canceled subscriber blocked delivery")
	}
}

func TestCloseStopsBusWorkersWithoutCancelingSession(t *testing.T) {
	for range 10 {
		a := newMetadataTestApp(t)
		handle := &projectionSession{id: "borrowed"}
		a.replaceSessionState(sessionState{handle: handle})
		ready := make(chan struct{})
		stopped := make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		go func() { defer close(stopped); a.Subscribe(ctx, func(any) {}, SubscribeOptions{Ready: ready}) }()
		<-ready
		a.Close()
		select {
		case <-a.busDone:
		case <-time.After(time.Second):
			t.Fatal("fanout/throttle workers leaked")
		}
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("subscriber leaked on Close")
		}
		cancel()
		assert.Empty(t, handle.cancelTurnID, "closing projection must not cancel execution")
	}
}

func TestInitializedBusDoesNotRequireCommandContext(t *testing.T) {
	a := &App{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a.initBus(ctx)
	lifetime := a.busLifetime()
	require.NotNil(t, lifetime)
	cancel()
	select {
	case <-lifetime.Done():
	default:
		t.Fatal("bus did not retain the supplied lifetime")
	}
}
