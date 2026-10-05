package runtime

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
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

func TestElicitationWaiter_ResolveBeforeReceiveIsNotLost(t *testing.T) {
	waiter := newElicitationWaiter()
	require.True(t, waiter.tryResolve(ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"k": "v"}}))
	assert.Equal(t, map[string]any{"k": "v"}, (<-waiter.ch).Content)
	assert.False(t, waiter.tryResolve(ElicitationResult{}), "duplicate response cannot win")
}

func TestElicitationWaiter_CancelWinsWhenFirst(t *testing.T) {
	waiter := newElicitationWaiter()
	require.True(t, waiter.tryCancel())
	assert.False(t, waiter.tryResolve(ElicitationResult{Action: tools.ElicitationActionAccept}))
}

func TestElicitationWaiter_ResolveWinsWhenFirst(t *testing.T) {
	waiter := newElicitationWaiter()
	require.True(t, waiter.tryResolve(ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"k": "v"}}))
	assert.False(t, waiter.tryCancel())
	assert.Equal(t, map[string]any{"k": "v"}, (<-waiter.ch).Content)
}

func TestElicitationWaiter_ConcurrentResolveCancelRace(t *testing.T) {
	for range 300 {
		waiter := newElicitationWaiter()
		var resolveWon, cancelWon atomic.Bool
		var workers sync.WaitGroup
		workers.Go(func() { resolveWon.Store(waiter.tryResolve(ElicitationResult{Action: tools.ElicitationActionAccept})) })
		workers.Go(func() { cancelWon.Store(waiter.tryCancel()) })
		workers.Wait()
		require.NotEqual(t, resolveWon.Load(), cancelWon.Load(), "exactly one terminal transition wins")
		if resolveWon.Load() {
			assert.Equal(t, tools.ElicitationActionAccept, (<-waiter.ch).Action)
		}
	}
}

func TestSessionElicitationSubscribersRouteToNearestOpenAncestor(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	parent := session.New(session.WithID(t.Name() + "/parent"))
	child := session.New(session.WithID(t.Name()+"/child"), session.WithParentID(parent.ID))
	detached := session.New(session.WithID(t.Name()+"/detached"), session.WithParentID(child.ID))
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
	open := session.New(session.WithID(t.Name() + "/open-root"))
	unrelated := session.New(session.WithID(t.Name() + "/unrelated-root"))
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
	parent := session.New(session.WithID(t.Name() + "/parent"))
	child := session.New(session.WithID(t.Name()+"/child"), session.WithParentID(parent.ID))
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
	sess := session.New(session.WithID(t.Name() + "/reentrant"))
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
	sess := session.New(session.WithID(t.Name() + "/full-app-bus"))
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
	parent := session.New(session.WithID(t.Name() + "/parent"))
	child := session.New(session.WithID(t.Name()+"/child"), session.WithParentID(parent.ID))
	rt.sessionDrivers.Get(parent)
	rt.sessionDrivers.Get(child)

	parentEvents := make(chan Event, 2)
	cancelParent, _ := rt.SubscribeSessionElicitations(parent.ID, "", func(event Event) bool {
		parentEvents <- event
		req := event.(*ElicitationRequestEvent)
		handle, err := rt.SessionByID(req.SessionID)
		if err != nil {
			return false
		}
		return handle.Respond(t.Context(), InteractionResponse{InteractionID: req.RequestID, Kind: InteractionElicitation, ElicitationID: req.ElicitationID, Elicitation: ElicitationResult{Action: tools.ElicitationActionAccept}}) == nil
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
	assert.Zero(t, sessionElicitationCountForTest(rt), "rejected background delivery must not leave a waiter")

	cancelChild()
	result, err = rt.elicitationHandler(ctx, &mcp.ElicitParams{Message: "second"})
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionAccept, result.Action)
	require.Len(t, parentEvents, 1, "after child removal, the next request hands off to its ancestor")
	assert.Empty(t, childAttempts)
	assert.Zero(t, sessionElicitationCountForTest(rt))
}

func TestElicitationHandler_BackgroundUnregisterAfterAvailabilityFastDeclines(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New(session.WithID(t.Name() + "/background"))
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
	assert.Zero(t, sessionElicitationCountForTest(rt), "fast decline must abandon its waiter")
}

func TestSessionElicitationSubscribersConcurrentLifecycle(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)
	sess := session.New(session.WithID(t.Name() + "/shared"))
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

func TestElicitationHandler_ScopedSinkReceivesAndResponds(t *testing.T) {
	t.Parallel()

	rt := newElicitationTestRuntime(t)

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

	// The transport subscription receives an addressable owner interaction.
	var ev *ElicitationRequestEvent
	select {
	case e := <-sinkCalled:
		ev = e.(*ElicitationRequestEvent)
	case <-time.After(1 * time.Second):
		t.Fatal("the scoped subscription must receive the request")
	}

	respondToElicitation(t, rt, ev, ElicitationResult{Action: tools.ElicitationActionAccept, Content: nil})

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.Equal(t, tools.ElicitationActionAccept, got.result.Action)
	case <-time.After(1 * time.Second):
		t.Fatal("the handler must complete after its scoped response")
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
	_, err := rt.CreateSession(t.Context(), session.New(session.WithID(t.Name()+"/bg-sess-1")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	ctx = genai.WithConversationID(ctx, t.Name()+"/bg-sess-1")

	result, err := rt.elicitationHandler(ctx, &mcp.ElicitParams{Message: "need sudo password"})
	require.NoError(t, err)
	assert.Equal(t, tools.ElicitationActionDecline, result.Action,
		"a background session with no UI sink must fast-decline instead of blocking forever")

	notes := elicitationDeclineNotesForTest(t, rt, t.Name()+"/bg-sess-1")
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
	_, err := rt.CreateSession(t.Context(), session.New(session.WithID("bg-sess-2")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
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
	assert.Empty(t, elicitationDeclineNotesForTest(t, rt, "bg-sess-2"), "must not fast-decline once a sink is registered")
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
		handle, err := rt.SessionByID(e.SessionID)
		require.NoError(t, err)
		ok := handle.Respond(t.Context(), InteractionResponse{InteractionID: e.RequestID, Kind: InteractionElicitation, ElicitationID: e.ElicitationID, Elicitation: ElicitationResult{Action: tools.ElicitationActionAccept, Content: map[string]any{"answered": true}}}) == nil
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
