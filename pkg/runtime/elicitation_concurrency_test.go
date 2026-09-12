package runtime

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/tools"
	agenttool "github.com/docker/docker-agent/pkg/tools/builtin/agent"
	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
)

// --- elicitationWaiters: unit + concurrency regression tests (#3584) ---

func TestElicitationWaiters_ResolveRoutesToCorrectID(t *testing.T) {
	t.Parallel()

	var w elicitationWaiters
	wtA := w.register("a")
	wtB := w.register("b")

	require.True(t, w.resolve("b", ElicitationResult{Action: tools.ElicitationActionDecline}))
	require.True(t, w.resolve("a", ElicitationResult{Action: tools.ElicitationActionAccept}))

	select {
	case result := <-wtA.ch:
		assert.Equal(t, tools.ElicitationActionAccept, result.Action, "waiter a must receive a's response, not b's")
	default:
		t.Fatal("waiter a never received its response")
	}
	select {
	case result := <-wtB.ch:
		assert.Equal(t, tools.ElicitationActionDecline, result.Action, "waiter b must receive b's response, not a's")
	default:
		t.Fatal("waiter b never received its response")
	}
}

// TestElicitationWaiters_ResolveBeforeReceiveIsNotLost pins the TOCTOU fix:
// registering the waiter before the request event is emitted means a
// response that arrives before anyone has read from the channel is still
// captured (the channel is buffered), instead of the old shared unbuffered
// elicitationRequestCh's `default:` branch reporting "no elicitation request
// in progress". See TestElicitationHandler_TOCTOU_ResolveImmediatelyAfterRegister
// below for the same pin exercised through the real handler path.
func TestElicitationWaiters_ResolveBeforeReceiveIsNotLost(t *testing.T) {
	t.Parallel()

	var w elicitationWaiters
	wt := w.register("id-1")

	ok := w.resolve("id-1", ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"k": "v"}})
	require.True(t, ok, "resolve must succeed even though nothing has received from the channel yet")

	select {
	case result := <-wt.ch:
		assert.Equal(t, tools.ElicitationActionAccept, result.Action)
		assert.Equal(t, map[string]any{"k": "v"}, result.Content)
	default:
		t.Fatal("the buffered waiter channel should already hold the resolved result")
	}
}

func TestElicitationWaiters_ResolveUnknownIDReturnsFalse(t *testing.T) {
	t.Parallel()

	var w elicitationWaiters
	assert.False(t, w.resolve("missing", ElicitationResult{}))
}

// TestElicitationWaiters_ResolveSingle_AmbiguousWithMultiplePending verifies

func TestElicitationWaiters_Abandon(t *testing.T) {
	t.Parallel()

	var w elicitationWaiters
	wt := w.register("a")
	require.Equal(t, 1, elicitationWaiterCountForTest(&w))

	w.abandon("a", wt)
	assert.Equal(t, 0, elicitationWaiterCountForTest(&w))
	assert.False(t, w.resolve("a", ElicitationResult{}), "abandoned waiter must not be resolvable")
}

// TestElicitationWaiters_DuplicateWireIDsDoNotCollide pins #3584 review item
// 2a: registry keys are always internally-generated IDs, never the MCP wire
// ElicitationID, precisely because two independent MCP servers (e.g. two
// concurrent background jobs, each talking to their own server) can
// legitimately reuse the same wire ID. Registering two waiters under
// different (internal) IDs — even when both requests logically share one
// wire ID — must never let the second registration evict the first's
// channel.
func TestElicitationWaiters_DuplicateWireIDsDoNotCollide(t *testing.T) {
	t.Parallel()

	var w elicitationWaiters
	// Simulates two servers both using wire ID "1": elicitationHandler
	// always mints a fresh internal correlation ID regardless, so the two
	// registrations land under distinct keys.
	wtServerA := w.register("internal-a")
	wtServerB := w.register("internal-b")

	require.Equal(t, 2, elicitationWaiterCountForTest(&w), "distinct internal IDs must not collide even if the wire IDs would have")

	require.True(t, w.resolve("internal-a", ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"who": "a"}}))
	require.True(t, w.resolve("internal-b", ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"who": "b"}}))

	select {
	case result := <-wtServerA.ch:
		assert.Equal(t, map[string]any{"who": "a"}, result.Content, "server A's waiter must not have been evicted or overwritten")
	default:
		t.Fatal("server A's waiter never received its response")
	}
	select {
	case result := <-wtServerB.ch:
		assert.Equal(t, map[string]any{"who": "b"}, result.Content, "server B's waiter must not have been evicted or overwritten")
	default:
		t.Fatal("server B's waiter never received its response")
	}
}

// TestElicitationWaiter_CancelWinsWhenFirst pins the #3584 review item 2b
// cancellation-vs-response race: when the handler's ctx.Done() branch wins
// the terminal-state CAS first, a subsequent resolve() must be told it lost
// (return false) instead of silently succeeding into a channel nobody will
// ever read again.
func TestElicitationWaiter_CancelWinsWhenFirst(t *testing.T) {
	t.Parallel()

	var w elicitationWaiters
	wt := w.register("a")

	require.True(t, w.cancel("a", wt), "cancel must win when nothing resolved yet")
	assert.False(t, w.resolve("a", ElicitationResult{Action: tools.ElicitationActionAccept}),
		"a resolve racing after cancel already won must not report success")
	assert.Equal(t, 0, elicitationWaiterCountForTest(&w), "cancel must remove the waiter from the registry")
}

// TestElicitationWaiter_ResolveWinsWhenFirst is the mirror image: resolve()
// wins the race first, so the handler's cancel() call must lose and report
// false, telling the handler to drain the value from the channel instead of
// discarding a response ResumeElicitation already reported as delivered.
func TestElicitationWaiter_ResolveWinsWhenFirst(t *testing.T) {
	t.Parallel()

	var w elicitationWaiters
	wt := w.register("a")

	require.True(t, w.resolve("a", ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"k": "v"}}))
	assert.False(t, w.cancel("a", wt), "cancel must lose once resolve already won the terminal-state race")

	select {
	case result := <-wt.ch:
		assert.Equal(t, map[string]any{"k": "v"}, result.Content, "the resolved value must still be retrievable by the loser of the race")
	default:
		t.Fatal("resolve's value must be in the channel even though cancel lost the race")
	}
}

// TestElicitationWaiters_ConcurrentRegisterResolveDeregister runs many
// concurrent request/response pairs through the registry to catch data races
// (run with -race) and confirm no response is ever misdelivered under
// concurrent load, mirroring the concurrent background-job scenario from
// the audit.
func TestElicitationWaiters_ConcurrentRegisterResolveDeregister(t *testing.T) {
	t.Parallel()

	var w elicitationWaiters
	const n = 200

	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			id := fmt.Sprintf("req-%d", i)
			wt := w.register(id)
			defer w.abandon(id, wt)

			done := make(chan struct{})
			go func() {
				defer close(done)
				ok := w.resolve(id, ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"i": i}})
				assert.True(t, ok)
			}()

			select {
			case result := <-wt.ch:
				assert.Equal(t, map[string]any{"i": i}, result.Content, "response must route back to its own request")
			case <-time.After(2 * time.Second):
				t.Errorf("waiter %s never received its response", id)
			}
			<-done
		})
	}
	wg.Wait()
}

// TestElicitationWaiters_ConcurrentResolveCancelRace hammers a single waiter
// with concurrent resolve/cancel attempts (run with -race) to confirm the
// terminal-state CAS lets exactly one of them win, never both and never
// neither.
func TestElicitationWaiters_ConcurrentResolveCancelRace(t *testing.T) {
	t.Parallel()

	const n = 300
	for i := range n {
		var w elicitationWaiters
		id := fmt.Sprintf("race-%d", i)
		wt := w.register(id)

		var wg sync.WaitGroup
		var resolveWon, cancelWon atomicBool
		wg.Add(2)
		go func() {
			defer wg.Done()
			if w.resolve(id, ElicitationResult{Action: tools.ElicitationActionAccept}) {
				resolveWon.set(true)
			}
		}()
		go func() {
			defer wg.Done()
			if w.cancel(id, wt) {
				cancelWon.set(true)
			}
		}()
		wg.Wait()

		require.NotEqual(t, resolveWon.get(), cancelWon.get(), "exactly one of resolve/cancel must win, never both or neither")
	}
}

// atomicBool is a tiny test-local helper; sync/atomic.Bool is available but
// spelling out set/get keeps the race-loop above terse.
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (a *atomicBool) set(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v = v
}

func (a *atomicBool) get() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

// --- elicitationBridge: bounded/non-blocking send (#3584 review item 1) ---

// TestElicitationBridge_SendBlocksUntilCtxDone pins the review-item-1 fix: an
// unbuffered, unconsumed bridge channel used to block send() forever with no
// way out. send() must now be bounded by ctx and release with ctx.Err()
// instead of hanging, and must never panic.
func TestElicitationBridge_SendBlocksUntilCtxDone(t *testing.T) {
	t.Parallel()

	var b elicitationBridge
	ch := make(chan Event) // unbuffered, nobody ever reads it
	b.swap(ch)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := b.send(ctx, Warning("hello", "agent"))
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.DeadlineExceeded, "send on a full/abandoned channel must release via ctx, not block forever")
	assert.Less(t, elapsed, 2*time.Second, "send must not block substantially past the ctx deadline")
}

func TestSessionElicitationSubscribersRouteToNearestOpenAncestor(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"), session.WithParentID(parent.ID))
	detached := session.New(session.WithID("detached"), session.WithParentID(child.ID))
	rt.sessionDrivers.Get(parent)
	rt.sessionDrivers.Get(child)
	rt.sessionDrivers.Get(detached)

	parentEvents := make(chan Event, 4)
	childEvents := make(chan Event, 4)
	fallbackEvents := make(chan Event, 4)
	cancelParent, _ := rt.SubscribeSessionElicitations(parent.ID, parent.ParentID, func(event Event) bool { parentEvents <- event; return true })
	defer cancelParent()
	cancelChild, _ := rt.SubscribeSessionElicitations(child.ID, child.ParentID, func(event Event) bool { childEvents <- event; return true })
	rt.OnElicitationRequest(func(event Event) { fallbackEvents <- event })

	emit := func(id string) Event {
		event := ElicitationRequest(id, "form", nil, "", id, "", id, nil, "root")
		rt.emitElicitationRequest(event)
		return event
	}
	parentEvent := emit(parent.ID)
	childEvent := emit(child.ID)
	detachedEvent := emit(detached.ID)

	assert.Equal(t, parentEvent, <-parentEvents)
	assert.Equal(t, childEvent, <-childEvents)
	assert.Equal(t, detachedEvent, <-childEvents, "a detached descendant belongs to its nearest subscribed ancestor")
	assert.Empty(t, fallbackEvents)
	assert.Empty(t, parentEvents)
	assert.Empty(t, childEvents)

	cancelChild()
	emittedAfterClose := emit(detached.ID)
	assert.Equal(t, emittedAfterClose, <-parentEvents,
		"closing the child App deterministically promotes ownership to the parent")

	cancelParent()
	unknown := emit("server-only")
	assert.Equal(t, unknown, <-fallbackEvents, "non-App/server embedders retain the singleton fallback sink")
}

func TestSessionElicitationSinkAvailabilityIsRouteAware(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	open := session.New(session.WithID("open-root"))
	unrelated := session.New(session.WithID("unrelated-root"))
	rt.sessionDrivers.Get(open)
	rt.sessionDrivers.Get(unrelated)
	cancel, _ := rt.SubscribeSessionElicitations(open.ID, "", func(Event) bool { return true })
	defer cancel()

	assert.True(t, rt.hasElicitationSink(open.ID))
	assert.False(t, rt.hasElicitationSink(unrelated.ID),
		"an unrelated root must fast-decline rather than wait on another App's sink")
}

func TestSessionElicitationUnsubscribeWaitsForSelectedDelivery(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"), session.WithParentID(parent.ID))
	rt.sessionDrivers.Get(parent)
	rt.sessionDrivers.Get(child)

	parentEvents := make(chan Event, 1)
	cancelParent, _ := rt.SubscribeSessionElicitations(parent.ID, "", func(event Event) bool { parentEvents <- event; return true })
	defer cancelParent()
	entered := make(chan struct{})
	release := make(chan struct{})
	childEvents := make(chan Event, 1)
	cancelChild, _ := rt.SubscribeSessionElicitations(child.ID, parent.ID, func(event Event) bool {
		close(entered)
		<-release
		childEvents <- event
		return true
	})

	event := ElicitationRequest("child", "form", nil, "", "eid", "", child.ID, nil, "root")
	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		rt.emitElicitationRequest(event)
	}()
	<-entered

	unsubscribed := make(chan struct{})
	go func() {
		cancelChild()
		close(unsubscribed)
	}()
	select {
	case <-unsubscribed:
		t.Fatal("unsubscribe returned while its selected callback was still in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-emitted
	<-unsubscribed
	assert.Equal(t, event, <-childEvents, "the selected delivery completes before close returns")
	assert.Empty(t, parentEvents)

	rt.emitElicitationRequest(event)
	assert.Equal(t, event, <-parentEvents, "later requests deterministically hand off to the ancestor")
	assert.Empty(t, childEvents)
}

func TestSessionElicitationCallbackMayReenterRegistry(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New(session.WithID("reentrant"))
	rt.sessionDrivers.Get(sess)
	done := make(chan struct{})
	cancel, _ := rt.SubscribeSessionElicitations(sess.ID, "", func(Event) bool {
		rt.OnElicitationRequest(func(Event) {})
		nestedCancel, _ := rt.SubscribeSessionElicitations("nested", "", func(Event) bool { return true })
		nestedCancel()
		close(done)
		return true
	})
	defer cancel()

	rt.emitElicitationRequest(ElicitationRequest("request", "form", nil, "", "eid", "", sess.ID, nil, "root"))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback deadlocked while reentering the sink registry")
	}
}

func TestSessionElicitationUnsubscribeCancelsBlockedCallback(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New(session.WithID("full-app-bus"))
	rt.sessionDrivers.Get(sess)
	ctx, cancelCtx := context.WithCancel(t.Context())
	bus := make(chan Event, 1)
	bus <- Warning("fill", "root")
	entered := make(chan struct{})
	cancelSink, _ := rt.SubscribeSessionElicitations(sess.ID, "", func(event Event) bool {
		close(entered)
		select {
		case bus <- event:
		case <-ctx.Done():
		}
		return ctx.Err() == nil
	})

	emitted := make(chan struct{})
	go func() {
		defer close(emitted)
		rt.emitElicitationRequest(ElicitationRequest("request", "form", nil, "", "eid", "", sess.ID, nil, "root"))
	}()
	<-entered
	cancelCtx() // App teardown cancels its bridge before waiting on unsubscribe.
	cancelSink()
	select {
	case <-emitted:
	case <-time.After(time.Second):
		t.Fatal("full App bus prevented teardown from releasing the callback lease")
	}
}

func TestElicitationHandler_RejectedScopedDeliveryFastDeclinesThenHandsOff(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	parent := session.New(session.WithID("parent"))
	child := session.New(session.WithID("child"), session.WithParentID(parent.ID))
	rt.sessionDrivers.Get(parent)
	rt.sessionDrivers.Get(child)

	parentEvents := make(chan Event, 2)
	cancelParent, _ := rt.SubscribeSessionElicitations(parent.ID, "", func(event Event) bool {
		parentEvents <- event
		req := event.(*ElicitationRequestEvent)
		return rt.elicitationWaiters.resolve(req.ElicitationID, ElicitationResult{Action: tools.ElicitationActionAccept})
	})
	defer cancelParent()
	childAttempts := make(chan Event, 1)
	cancelChild, _ := rt.SubscribeSessionElicitations(child.ID, parent.ID, func(event Event) bool {
		childAttempts <- event
		return false // App bridge context was canceled/full; nothing was enqueued.
	})

	ctx := mcptools.WithoutInteractivePrompts(t.Context())
	ctx = genai.WithConversationID(ctx, child.ID)
	result, err := rt.elicitationHandler(ctx, &mcp.ElicitParams{Message: "first"})
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionDecline, result.Action)
	require.Len(t, childAttempts, 1, "the stale child lease rejected actual delivery")
	<-childAttempts
	assert.Empty(t, parentEvents, "a rejected selected callback must not cross-deliver in the same emission")
	assert.Zero(t, elicitationWaiterCountForTest(&rt.elicitationWaiters), "rejected background delivery must not leave a waiter")

	cancelChild()
	result, err = rt.elicitationHandler(ctx, &mcp.ElicitParams{Message: "second"})
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionAccept, result.Action)
	require.Len(t, parentEvents, 1, "after child removal, the next request hands off to its ancestor")
	assert.Empty(t, childAttempts)
	assert.Zero(t, elicitationWaiterCountForTest(&rt.elicitationWaiters))
}

func TestElicitationHandler_BackgroundUnregisterAfterAvailabilityFastDeclines(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New(session.WithID("background"))
	rt.sessionDrivers.Get(sess)
	cancel, _ := rt.SubscribeSessionElicitations(sess.ID, "", func(Event) bool { return true })
	require.True(t, rt.hasElicitationSink(sess.ID), "barrier: availability was observed before unregister")
	cancel()

	ctx := mcptools.WithoutInteractivePrompts(t.Context())
	ctx = genai.WithConversationID(ctx, sess.ID)
	result, err := rt.elicitationHandler(ctx, &mcp.ElicitParams{Message: "confirm?"})
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionDecline, result.Action,
		"atomic route acquisition must detect that the previously observed route is gone")
	assert.Zero(t, elicitationWaiterCountForTest(&rt.elicitationWaiters), "fast decline must abandon its waiter")
}

func TestSessionElicitationSubscribersConcurrentLifecycle(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New(session.WithID("shared"))
	rt.sessionDrivers.Get(sess)
	event := ElicitationRequest("request", "form", nil, "", "eid", "", sess.ID, nil, "root")

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			cancel, _ := rt.SubscribeSessionElicitations(sess.ID, "", func(Event) bool { return true })
			cancel()
		}()
		go func() {
			defer wg.Done()
			rt.emitElicitationRequest(event)
		}()
	}
	wg.Wait()
}

// TestElicitationBridge_SendNeverBlocksReliableSink is the end-to-end version
// of the review-item-1 fix: a wedged bridge channel must not delay — let
// alone block — elicitationHandler's reliable OnElicitationRequest sink
// delivery or its subsequent wait for a response. Before the fix, the bridge
// send was awaited synchronously and BEFORE the sink call, so an abandoned
// channel meant the sink (and thus the whole request) never even started.
func TestElicitationBridge_SendNeverBlocksReliableSink(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)

	// Wedge the bridge: swap in an unbuffered channel with no reader, as if
	// a concurrent RunStream's swap left a dead consumer behind.
	wedged := make(chan Event)
	rt.elicitation.swap(wedged)

	sinkCalled := make(chan Event, 1)
	rt.OnElicitationRequest(func(ev Event) { sinkCalled <- ev })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	type handlerResult struct {
		result tools.ElicitationResult
		err    error
	}
	done := make(chan handlerResult, 1)
	go func() {
		result, err := rt.elicitationHandler(ctx, &mcp.ElicitParams{Message: "confirm?"})
		done <- handlerResult{result, err}
	}()

	// The sink must fire almost immediately, regardless of the wedged bridge.
	var ev *ElicitationRequestEvent
	select {
	case e := <-sinkCalled:
		ev = e.(*ElicitationRequestEvent)
	case <-time.After(1 * time.Second):
		t.Fatal("the reliable sink must not be blocked by a wedged bridge channel")
	}

	respondToElicitation(t, rt, ev, ElicitationResult{Action: tools.ElicitationActionAccept, Content: nil})

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, tools.ElicitationActionAccept, got.result.Action)
	case <-time.After(1 * time.Second):
		t.Fatal("elicitationHandler must not be blocked by a wedged bridge channel")
	}
}

// --- elicitationHandler: headless fast-decline (#3584 item 5) ---

func TestElicitationHandler_HeadlessBackgroundFastDeclines(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)

	// No OnElicitationRequest sink registered, and the context is marked
	// non-interactive the way runStreamLoop marks a background
	// (run_background_agent) session's context (#3200).
	ctx := mcptools.WithoutInteractivePrompts(t.Context())
	ctx = genai.WithConversationID(ctx, "bg-sess-1")

	result, err := rt.elicitationHandler(ctx, &mcp.ElicitParams{Message: "need sudo password"})
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionDecline, result.Action,
		"a background session with no UI sink must fast-decline instead of blocking forever")

	notes := rt.elicitationDeclines.drain("bg-sess-1")
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "need sudo password")
}

// TestElicitationHandler_BackgroundWithSinkStillWaitsForResponse verifies
// that registering an OnElicitationRequest sink is enough to opt a
// background-session elicitation back into the normal wait-for-response
// path instead of being fast-declined.
func TestElicitationHandler_BackgroundWithSinkStillWaitsForResponse(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)

	received := make(chan Event, 1)
	rt.OnElicitationRequest(func(ev Event) { received <- ev })

	ctx := mcptools.WithoutInteractivePrompts(t.Context())
	ctx = genai.WithConversationID(ctx, "bg-sess-2")

	type handlerResult struct {
		result tools.ElicitationResult
		err    error
	}
	done := make(chan handlerResult, 1)
	go func() {
		result, err := rt.elicitationHandler(ctx, &mcp.ElicitParams{Message: "confirm?"})
		done <- handlerResult{result, err}
	}()

	var ev *ElicitationRequestEvent
	select {
	case e := <-received:
		ev = e.(*ElicitationRequestEvent)
	case <-time.After(2 * time.Second):
		t.Fatal("sink never received the elicitation request")
	}
	assert.Equal(t, "bg-sess-2", ev.SessionID, "the event must carry the originating (sub-)session ID")

	respondToElicitation(t, rt, ev, ElicitationResult{Action: tools.ElicitationActionAccept, Content: nil})

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, tools.ElicitationActionAccept, got.result.Action)
	case <-time.After(2 * time.Second):
		t.Fatal("elicitationHandler never returned")
	}
	assert.Empty(t, rt.elicitationDeclines.drain("bg-sess-2"), "must not fast-decline once a sink is registered")
}

// TestElicitationHandler_TOCTOU_ResolveImmediatelyAfterRegister promotes the
// former helper-level TOCTOU pin (TestElicitationWaiters_ResolveBeforeReceiveIsNotLost)
// to exercise the real elicitationHandler code path end-to-end: the sink
// fires synchronously before the handler ever reaches its response select,
// so a caller that resolves the instant it observes the sink-delivered event
// — beating the handler to its `select` — must still have its response
// delivered rather than racing the old shared-channel `default:` branch.
func TestElicitationHandler_TOCTOU_ResolveImmediatelyAfterRegister(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)

	sinkDone := make(chan struct{})
	rt.OnElicitationRequest(func(ev Event) {
		// Resolve from inside the sink callback itself, i.e. before
		// elicitationHandler's goroutine has any chance to reach its
		// `select` on the waiter channel. This is the tightest possible
		// version of the TOCTOU window.
		e := ev.(*ElicitationRequestEvent)
		ok := rt.elicitationWaiters.resolve(e.ElicitationID, ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"answered": true}})
		assert.True(t, ok, "resolve issued synchronously from the sink callback must still find the just-registered waiter")
		close(sinkDone)
	})

	result, err := rt.elicitationHandler(t.Context(), &mcp.ElicitParams{Message: "confirm?"})
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionAccept, result.Action)
	assert.Equal(t, map[string]any{"answered": true}, result.Content)
	<-sinkDone
}

// --- Full-stack regression: concurrent background-job elicitations (#3584) ---

// elicitingToolSet is a minimal toolset whose one tool blocks on an MCP
// elicitation via the handler ConfigureHandlers wires onto it — mirroring
// how a real MCP toolset elicits mid-tool-call — without needing a real MCP
// server. Used to reproduce the reported bug: elicitations raised from
// multiple concurrent background jobs (run_background_agent) must all
// surface and each response must reach its own waiter.
type elicitingToolSet struct {
	mu      sync.Mutex
	handler tools.ElicitationHandler
	message string
	// received captures the ElicitationResult the tool call got back, so
	// tests can verify the response that reached this specific worker
	// without having to scrape it back out of the model transcript.
	received  tools.ElicitationResult
	gotResult bool
}

var _ tools.Elicitable = (*elicitingToolSet)(nil)

func (e *elicitingToolSet) SetElicitationHandler(handler tools.ElicitationHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handler = handler
}

func (e *elicitingToolSet) snapshot() (tools.ElicitationResult, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.received, e.gotResult
}

func (e *elicitingToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{
		Name: "ask_user",
		Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			e.mu.Lock()
			handler := e.handler
			e.mu.Unlock()
			if handler == nil {
				return tools.ResultError("no elicitation handler configured"), nil
			}
			result, err := handler(ctx, &mcp.ElicitParams{Message: e.message})
			if err != nil {
				return nil, err
			}
			e.mu.Lock()
			e.received = result
			e.gotResult = true
			e.mu.Unlock()
			return tools.ResultSuccess(fmt.Sprintf("%s:%v", result.Action, result.Content)), nil
		},
	}}, nil
}

// TestConcurrentBackgroundElicitations_AllSurfaceAndRouteToCorrectWaiter is
// the end-to-end regression test for issue #3584 / audit finding A6: two
// concurrent background jobs (as run_background_agent spawns them) each
// raise an elicitation mid-tool-call. Before the fix, the single-slot
// elicitationBridge and shared elicitationRequestCh meant: (a) runCollecting
// silently dropped the ElicitationRequestEvent, so nothing was ever
// displayed, and (b) even if something had displayed it, a response could be
// delivered to the wrong waiter. This asserts both jobs surface via the new
// OnElicitationRequest sink — exactly once each, with no dedupe map required
// (runCollecting no longer re-forwards a bridge-observed copy; see #3584
// review item 5) — and each receives its own, non-swapped response.
func TestConcurrentBackgroundElicitations_AllSurfaceAndRouteToCorrectWaiter(t *testing.T) {
	t.Parallel()

	newWorker := func(name, question string) (*agent.Agent, *elicitingToolSet) {
		ts := &elicitingToolSet{message: question}
		toolCallStream := newStreamBuilder().AddToolCallWithStop("call_1", "ask_user", "{}").Build()
		followUpStream := newStreamBuilder().AddContent("done").AddStopWithUsage(5, 5).Build()
		prov := &queueProvider{id: "test/mock-model", streams: []chat.MessageStream{toolCallStream, followUpStream}}
		a := agent.New(name, "worker", agent.WithModel(prov), agent.WithToolSets(ts))
		return a, ts
	}

	worker1, ts1 := newWorker("worker1", "worker1 needs input")
	worker2, ts2 := newWorker("worker2", "worker2 needs input")
	root := agent.New("root", "root", agent.WithModel(&mockProvider{id: "test/mock-model", stream: &mockStream{}}))
	agent.WithSubAgents(worker1, worker2)(root)

	tm := team.New(team.WithAgents(root, worker1, worker2))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)

	// deliveries counts sink invocations per ElicitationID. Each ID must be
	// delivered exactly once: any count > 1 means the exactly-once guarantee
	// (elicitationHandler is the sole caller of emitElicitationRequest) has
	// regressed, since nothing else in the App/runtime layer masks
	// duplicates any more.
	var mu sync.Mutex
	deliveries := make(map[string]int)
	requests := make(map[string]*ElicitationRequestEvent)
	rt.OnElicitationRequest(func(ev Event) {
		req := ev.(*ElicitationRequestEvent)
		mu.Lock()
		defer mu.Unlock()
		deliveries[req.ElicitationID]++
		requests[req.ElicitationID] = req
	})

	results := make(chan *agenttool.RunResult, 2)
	launch := func(agentName string) {
		go func() {
			parent := session.New(session.WithUserMessage("go"), session.WithToolsApproved(true))
			res := rt.RunAgent(t.Context(), agenttool.RunParams{
				AgentName:     agentName,
				Task:          "do it",
				ParentSession: parent,
			})
			results <- res
		}()
	}
	launch("worker1")
	launch("worker2")

	// Wait until both elicitations have surfaced, then respond to each by
	// its own ID with a distinguishable payload so a swapped response would
	// be caught by the assertions below.
	var reqs map[string]*ElicitationRequestEvent
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		reqs = make(map[string]*ElicitationRequestEvent, len(requests))
		maps.Copy(reqs, requests)
		return len(reqs) == 2
	}, 5*time.Second, 10*time.Millisecond, "both concurrent background elicitations must surface via the sink")

	mu.Lock()
	for id, count := range deliveries {
		assert.Equal(t, 1, count, "elicitation %s must be delivered to the sink exactly once", id)
	}
	mu.Unlock()

	var worker1ID, worker2ID string
	for id, ev := range reqs {
		switch ev.Message {
		case "worker1 needs input":
			worker1ID = id
		case "worker2 needs input":
			worker2ID = id
		}
	}
	require.NotEmpty(t, worker1ID, "worker1's elicitation must have surfaced")
	require.NotEmpty(t, worker2ID, "worker2's elicitation must have surfaced")
	require.NotEqual(t, worker1ID, worker2ID, "concurrent elicitations must get distinct correlation IDs")

	respondToElicitation(t, rt, reqs[worker1ID], ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"answer": "1"}})
	respondToElicitation(t, rt, reqs[worker2ID], ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"answer": "2"}})

	var got []*agenttool.RunResult
	for range 2 {
		select {
		case r := <-results:
			got = append(got, r)
		case <-time.After(5 * time.Second):
			t.Fatal("a background job never completed after its elicitation was resumed")
		}
	}
	for _, r := range got {
		require.Empty(t, r.ErrMsg, "background job must not fail")
	}

	worker1Result, ok1 := ts1.snapshot()
	require.True(t, ok1, "worker1's tool call must have received an elicitation result")
	worker2Result, ok2 := ts2.snapshot()
	require.True(t, ok2, "worker2's tool call must have received an elicitation result")

	assert.Equal(t, map[string]any{"answer": "1"}, worker1Result.Content,
		"worker1 must receive its own response, not worker2's (no swap)")
	assert.Equal(t, map[string]any{"answer": "2"}, worker2Result.Content,
		"worker2 must receive its own response, not worker1's (no swap)")
}
