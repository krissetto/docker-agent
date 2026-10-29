package app

import (
	"context"
	"fmt"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func invMemApp(ctx context.Context) *App {
	return &App{ctx: func() context.Context { return ctx }, events: make(chan any, 128), throttleDuration: 50 * time.Millisecond}
}

func invMemRegister(a *App, ctx context.Context, ch chan any) {
	a.subsMu.Lock()
	defer a.subsMu.Unlock()
	if a.subscriberDone == nil {
		a.subscriberDone = make(map[chan any]<-chan struct{})
	}
	a.subs = append(a.subs, ch)
	a.subscriberDone[ch] = ctx.Done()
}

func invMemSource(n int) []any {
	src := make([]any, n)
	for i := range src {
		src[i] = runtime.PendingUserMessageCanceled("s", "turn", i)
	}
	return src
}

func invMemAlive[T any](refs []weak.Pointer[T]) int {
	alive := 0
	for _, ref := range refs {
		if ref.Value() != nil {
			alive++
		}
	}
	return alive
}

func invMemRunFanout(tb testing.TB, subs int, src []any) {
	tb.Helper()
	ctx, cancel := context.WithCancel(tb.Context())
	defer cancel()
	a := invMemApp(ctx)
	var got atomic.Int64
	for range subs {
		ch := make(chan any, 1024)
		invMemRegister(a, ctx, ch)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-ch:
					got.Add(1)
				}
			}
		}()
	}
	a.startFanOut()
	for _, ev := range src {
		a.events <- ev
	}
	want := int64(subs * len(src))
	deadline := time.Now().Add(60 * time.Second)
	for got.Load() < want {
		if time.Now().After(deadline) {
			tb.Fatalf("fanout delivered %d of %d", got.Load(), want)
		}
		goruntime.Gosched()
	}
}

func TestInvMemFanoutDeliversEveryEventToEveryObserverInOrder(t *testing.T) {
	const events = 1000
	for _, subs := range []int{1, 8, 64} {
		ctx, cancel := context.WithCancel(t.Context())
		a := invMemApp(ctx)
		src := invMemSource(events)
		received := make([][]any, subs)
		done := make(chan int, subs)
		for i := range subs {
			ch := make(chan any, 1024)
			invMemRegister(a, ctx, ch)
			go func() {
				defer func() { done <- i }()
				for range events {
					received[i] = append(received[i], <-ch)
				}
			}()
		}
		a.startFanOut()
		for _, ev := range src {
			a.events <- ev
		}
		for range subs {
			<-done
		}
		cancel()
		for i := range subs {
			require.Len(t, received[i], events, "observer %d", i)
			for j := range events {
				require.Equal(t, src[j], received[i][j], "observer %d event %d", i, j)
			}
		}
	}
}

func TestInvMemStalledObserverHoldsFanoutUntilCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := invMemApp(ctx)
	stalledCtx, stop := context.WithCancel(ctx)
	defer stop()
	stalled := make(chan any, 1)
	witness := make(chan any, 16)
	invMemRegister(a, stalledCtx, stalled)
	invMemRegister(a, ctx, witness)
	a.startFanOut()

	first := runtime.StreamStarted("s", "root")
	second := runtime.StreamStopped("s", "root", "normal")
	a.events <- first
	require.Eventually(t, func() bool { return len(stalled) == 1 }, time.Second, time.Millisecond)
	a.events <- second

	require.Equal(t, first, <-witness)
	select {
	case got := <-witness:
		t.Fatalf("witness received %v while an earlier observer was stalled", got)
	case <-time.After(100 * time.Millisecond):
	}
	stop()
	select {
	case got := <-witness:
		assert.Equal(t, second, got)
	case <-time.After(time.Second):
		t.Fatal("witness not delivered after stalled observer canceled")
	}
}

func TestInvMemSubscribeCancelCyclesReleaseRegistry(t *testing.T) {
	ctx, cancelBus := context.WithCancel(t.Context())
	defer cancelBus()
	a := invMemApp(ctx)
	baseline := goruntime.NumGoroutine()
	for range 300 {
		subCtx, cancel := context.WithCancel(ctx)
		ready := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			a.Subscribe(subCtx, func(any) {}, SubscribeOptions{Ready: ready})
		}()
		<-ready
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Subscribe did not return after its context was canceled")
		}
	}
	a.subsMu.Lock()
	subs, doneMap := len(a.subs), len(a.subscriberDone)
	a.subsMu.Unlock()
	assert.Zero(t, subs, "subscriber slice retained canceled channels")
	assert.Zero(t, doneMap, "subscriberDone retained canceled channels")
	require.Eventually(t, func() bool { return goruntime.NumGoroutine() <= baseline+4 }, 2*time.Second, 10*time.Millisecond,
		"goroutines after cycles: %d baseline %d", goruntime.NumGoroutine(), baseline)
}

// Flushed events must not stay reachable through the throttle buffer's backing array.
func TestInvMemThrottleReleasesFlushedEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := invMemApp(ctx)
	a.throttleDuration = time.Hour
	in := make(chan any)
	out := a.throttleEvents(ctx, in)

	const burst = 2000
	refs := make([]weak.Pointer[runtime.AgentChoiceEvent], 0, burst)
	go func() {
		for i := range burst {
			ev := runtime.AgentChoice("root", "s", fmt.Sprintf("chunk-%06d", i)).(*runtime.AgentChoiceEvent)
			refs = append(refs, weak.Make(ev))
			in <- ev
		}
		in <- runtime.StreamStopped("s", "root", "normal")
	}()
	for {
		if _, ok := (<-out).(*runtime.StreamStoppedEvent); ok {
			break
		}
	}
	goruntime.GC()
	goruntime.GC()
	afterFlush := invMemAlive(refs)
	t.Logf("burst=%d unique_payloads alive_after_flush=%d", burst, afterFlush)
	assert.Zero(t, afterFlush, "flushed events must be released once the window is emitted")

	cancel()
	for range out {
	}
	goruntime.GC()
	goruntime.GC()
	afterCancel := invMemAlive(refs)
	t.Logf("alive_after_cancel=%d", afterCancel)
	assert.Zero(t, afterCancel, "cancelled throttle goroutine must release its buffer")
}

func TestInvMemFanoutToleratesSubscriptionChurn(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := invMemApp(ctx)
	const events = 200
	src := invMemSource(events)
	stable := make(chan any, events)
	invMemRegister(a, ctx, stable)
	a.fanoutOnce.Do(a.startFanOut)
	churned := make(chan struct{})
	go func() {
		defer close(churned)
		for range 100 {
			subCtx, cancelSub := context.WithCancel(ctx)
			ready := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				a.Subscribe(subCtx, func(any) {}, SubscribeOptions{Ready: ready})
			}()
			<-ready
			cancelSub()
			<-done
		}
	}()
	for _, ev := range src {
		a.events <- ev
	}
	<-churned
	for i := range events {
		select {
		case got := <-stable:
			require.Equal(t, src[i], got, "stable observer event %d", i)
		case <-time.After(5 * time.Second):
			t.Fatalf("stable observer missed event %d", i)
		}
	}
}

func BenchmarkInvMemFanout(b *testing.B) {
	for _, subs := range []int{1, 8, 64} {
		for _, events := range []int{1000, 5000, 20000} {
			b.Run(fmt.Sprintf("subs=%d/events=%d", subs, events), func(b *testing.B) {
				src := invMemSource(events)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					invMemRunFanout(b, subs, src)
				}
				b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*events), "ns/event")
			})
		}
	}
}

func BenchmarkInvMemFanoutSourceIncluded(b *testing.B) {
	const subs, events = 8, 5000
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		invMemRunFanout(b, subs, invMemSource(events))
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*events), "ns/event")
}

func TestInvMemDeliverSkipsSubscriberRemovedAfterSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := invMemApp(ctx)
	removed := make(chan any, 1)
	removed <- "stale"
	witness := make(chan any, 4)
	invMemRegister(a, ctx, witness)
	snapshot := []chan any{removed, witness}
	delivered := make(chan bool, 1)
	go func() { delivered <- a.deliver(ctx, snapshot, "first") }()
	select {
	case ok := <-delivered:
		require.True(t, ok)
	case <-time.After(time.Second):
		t.Fatal("delivery blocked on a removed subscriber's full channel")
	}
	require.Equal(t, "first", <-witness)
	require.True(t, a.deliver(ctx, snapshot, "second"), "bus must keep progressing after a stale snapshot entry")
	require.Equal(t, "second", <-witness)
}

func TestInvMemDeliverUnblocksCanceledFullSubscriber(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := invMemApp(ctx)
	subCtx, stop := context.WithCancel(ctx)
	full := make(chan any, 1)
	full <- "stale"
	invMemRegister(a, subCtx, full)
	witness := make(chan any, 4)
	invMemRegister(a, ctx, witness)
	stop()
	delivered := make(chan bool, 1)
	go func() { delivered <- a.deliver(ctx, []chan any{full, witness}, "msg") }()
	select {
	case ok := <-delivered:
		require.True(t, ok)
	case <-time.After(time.Second):
		t.Fatal("canceled full subscriber stalled delivery")
	}
	require.Equal(t, "msg", <-witness)
}
