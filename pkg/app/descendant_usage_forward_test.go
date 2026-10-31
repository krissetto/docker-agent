package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestDescendantUsageForwardsOnlyCurrentEpochChildUsage(t *testing.T) {
	a := &App{events: make(chan any, 4)}
	a.bridgeEpoch.Store(7)
	root := session.New(session.WithID("root"))
	root.SetUsage(11, 5)
	root.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "answer", Cost: 0.25}))
	a.replaceSessionState(sessionState{session: root})
	ownBefore, totalBefore := root.OwnCost(), root.TotalCost()
	watch := newSeedWatch()
	childOwn := 0.5
	child := &runtime.TokenUsageEvent{SessionID: "child", Usage: &runtime.Usage{ContextLength: 45, Cost: childOwn, OwnCost: &childOwn}}

	a.forwardDescendantUsage(t.Context(), watch, runtime.SessionEvent{SessionID: "child", Event: child}, 7)
	require.Len(t, a.events, 1)
	forwarded, ok := (<-a.events).(*runtime.TokenUsageEvent)
	require.True(t, ok)
	assert.Same(t, child, forwarded, "descendant usage is forwarded unchanged")
	assert.Equal(t, "child", forwarded.SessionID)
	assert.Equal(t, int64(45), forwarded.Usage.ContextLength)
	require.NotNil(t, forwarded.Usage.OwnCost)
	assert.InDelta(t, 0.5, *forwarded.Usage.OwnCost, 0)

	a.forwardDescendantUsage(t.Context(), watch, runtime.SessionEvent{SessionID: "root", Event: &runtime.TokenUsageEvent{SessionID: "root", Usage: &runtime.Usage{ContextLength: 10}}}, 7)
	a.forwardDescendantUsage(t.Context(), watch, runtime.SessionEvent{SessionID: "child", Gap: true, Event: child}, 7)
	a.forwardDescendantUsage(t.Context(), watch, runtime.SessionEvent{SessionID: "child", Event: &runtime.TokenUsageEvent{SessionID: "child"}}, 7)
	a.forwardDescendantUsage(t.Context(), watch, runtime.SessionEvent{SessionID: "child", Event: child}, 6)
	a.forwardDescendantUsage(t.Context(), watch, runtime.SessionEvent{Event: child}, 7)
	a.forwardDescendantUsage(t.Context(), watch, runtime.SessionEvent{SessionID: "child", Event: &runtime.TokenUsageEvent{SessionID: "sibling", Usage: child.Usage}}, 7)
	assert.Empty(t, a.events)

	input, output := a.Session().Usage()
	assert.Equal(t, int64(11), input)
	assert.Equal(t, int64(5), output)
	assert.InDelta(t, ownBefore, a.Session().OwnCost(), 0, "root own cost unchanged by descendant usage")
	assert.InDelta(t, totalBefore, a.Session().TotalCost(), 0, "root total cost unchanged: no double count")
}

func TestDescendantSeedableRequiresNonRootRecordedTokens(t *testing.T) {
	child := session.New(session.WithID("child"))
	assert.False(t, descendantSeedable(child, "root"), "no recorded tokens")
	child.SetUsage(300, 150)
	assert.True(t, descendantSeedable(child, "root"))
	assert.False(t, descendantSeedable(session.New(session.WithID("root")), "root"), "root is seeded by its own stream")
	assert.False(t, descendantSeedable(nil, "root"))
}

// agentInfoHandle stands in for a child session handle that reports bound agent info.
type agentInfoHandle struct {
	runtime.SessionHandle

	info runtime.SessionAgentInfo
	err  error
}

func (h *agentInfoHandle) SessionAgentInfo(context.Context) (runtime.SessionAgentInfo, error) {
	return h.info, h.err
}

// plainHandle exposes no agent info capability, like an unsupported peer.
type plainHandle struct{ runtime.SessionHandle }

type seedSessionsFake struct {
	runtime.SessionRuntime

	handles map[string]runtime.SessionHandle
	lookups []string
}

func (s *seedSessionsFake) SessionByID(id string) (runtime.SessionHandle, error) {
	s.lookups = append(s.lookups, id)
	if handle, ok := s.handles[id]; ok {
		return handle, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: id, Operation: "resolve"}
}

func TestSeedDescendantUsageUsesChildAgentContextLimit(t *testing.T) {
	child := session.New(session.WithID("child"))
	child.SetUsage(300, 150)
	sessions := &seedSessionsFake{handles: map[string]runtime.SessionHandle{
		"child": &agentInfoHandle{info: runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}},
	}}
	a := &App{events: make(chan any, 4), sessions: sessions}
	a.bridgeEpoch.Store(7)
	watch := newSeedWatch()

	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: child}, 7)
	require.Len(t, a.events, 1)
	seed, ok := (<-a.events).(*runtime.TokenUsageEvent)
	require.True(t, ok)
	assert.Equal(t, "child", seed.SessionID)
	assert.Equal(t, int64(450), seed.Usage.ContextLength)
	assert.Equal(t, int64(1000), seed.Usage.ContextLimit)
	require.NotNil(t, seed.Usage.OwnCost)

	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: session.New(session.WithID("root"))}, 7)
	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: session.New(session.WithID("idle"))}, 7)
	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: child}, 6)
	assert.Equal(t, []string{"child"}, sessions.lookups, "root, idle and stale-epoch snapshots never reach the handle")
	assert.Empty(t, a.events)
}

func TestSeedDescendantUsageUnsupportedOrFailedLookupsEmitNothing(t *testing.T) {
	child := session.New(session.WithID("child"))
	child.SetUsage(300, 150)
	cases := map[string]*seedSessionsFake{
		"unsupported handle": {handles: map[string]runtime.SessionHandle{"child": &plainHandle{}}},
		"handle error":       {handles: map[string]runtime.SessionHandle{"child": &agentInfoHandle{err: assert.AnError}}},
		"no agent info":      {handles: map[string]runtime.SessionHandle{"child": &agentInfoHandle{}}},
		"missing session":    {handles: map[string]runtime.SessionHandle{}},
	}
	for name, sessions := range cases {
		t.Run(name, func(t *testing.T) {
			a := &App{events: make(chan any, 1), sessions: sessions}
			a.bridgeEpoch.Store(7)
			a.seedDescendantUsage(t.Context(), newSeedWatch(), runtime.SessionSnapshot{Session: child}, 7)
			assert.Empty(t, a.events, "unsupported or failed agent info must not invent usage")
		})
	}
}

// epochShiftingHandle replaces the view while its agent-info lookup is in flight.
type epochShiftingHandle struct {
	runtime.SessionHandle

	app *App
}

func (h *epochShiftingHandle) SessionAgentInfo(context.Context) (runtime.SessionAgentInfo, error) {
	h.app.bridgeEpoch.Store(8)
	return runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}, nil
}

func TestSeedDescendantUsageDropsStaleEpochAfterLookup(t *testing.T) {
	child := session.New(session.WithID("child"))
	child.SetUsage(300, 150)
	a := &App{events: make(chan any, 2)}
	a.sessions = &seedSessionsFake{handles: map[string]runtime.SessionHandle{"child": &epochShiftingHandle{app: a}}}
	a.bridgeEpoch.Store(7)
	a.seedDescendantUsage(t.Context(), newSeedWatch(), runtime.SessionSnapshot{Session: child}, 7)
	assert.Empty(t, a.events, "an answer from a replaced view must not publish")
}

func TestSeedDescendantUsageLookupTimesOut(t *testing.T) {
	child := session.New(session.WithID("child"))
	child.SetUsage(300, 150)
	a := &App{events: make(chan any, 1), descendantSeedBudget: 20 * time.Millisecond, sessions: &seedSessionsFake{handles: map[string]runtime.SessionHandle{"child": &blockingInfoHandle{}}}}
	a.bridgeEpoch.Store(7)
	a.seedDescendantUsage(t.Context(), newSeedWatch(), runtime.SessionSnapshot{Session: child}, 7)
	assert.Empty(t, a.events)
}

type blockingInfoHandle struct{ runtime.SessionHandle }

func (h *blockingInfoHandle) SessionAgentInfo(ctx context.Context) (runtime.SessionAgentInfo, error) {
	<-ctx.Done()
	return runtime.SessionAgentInfo{}, ctx.Err()
}

func TestSeedDescendantUsagesCapsLookupsPerBaseline(t *testing.T) {
	handles := map[string]runtime.SessionHandle{}
	var snapshots []runtime.SessionSnapshot
	for i := range 40 {
		id := fmt.Sprintf("child-%02d", i)
		child := session.New(session.WithID(id))
		child.SetUsage(10, 5)
		snapshots = append(snapshots, runtime.SessionSnapshot{Session: child})
		handles[id] = &agentInfoHandle{info: runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}}
	}
	sessions := &seedSessionsFake{handles: handles}
	a := &App{events: make(chan any, 64), sessions: sessions}
	a.bridgeEpoch.Store(7)
	a.seedDescendantUsages(t.Context(), newSeedWatch(), snapshots, 7)
	assert.Len(t, sessions.lookups, maxDescendantSeeds)
	assert.Len(t, a.events, maxDescendantSeeds)
}

func TestConsumeTreeObservationSeedsSessionsAddedChild(t *testing.T) {
	child := session.New(session.WithID("child-added"))
	child.SetUsage(300, 150)
	sessions := &seedSessionsFake{handles: map[string]runtime.SessionHandle{
		"child-added": &agentInfoHandle{info: runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}},
	}}
	a := &App{events: make(chan any, 4), sessions: sessions}
	a.bridgeEpoch.Store(7)
	added := make(chan runtime.SessionSnapshot, 1)
	added <- runtime.SessionSnapshot{Session: child}
	close(added)
	a.consumeTreeObservation(t.Context(), newSeedWatch(), runtime.Observation{SessionsAdded: added}, 7, false)
	assert.Equal(t, []string{"child-added"}, sessions.lookups)
	require.Len(t, a.events, 1)
	seed, ok := (<-a.events).(*runtime.TokenUsageEvent)
	require.True(t, ok)
	assert.Equal(t, "child-added", seed.SessionID)
}

func TestConsumeTreeObservationSeedsTreeUpdateChild(t *testing.T) {
	child := session.New(session.WithID("child-update"))
	child.SetUsage(300, 150)
	sessions := &seedSessionsFake{handles: map[string]runtime.SessionHandle{
		"child-update": &agentInfoHandle{info: runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}},
	}}
	a := &App{events: make(chan any, 4), sessions: sessions}
	a.bridgeEpoch.Store(7)
	updates := make(chan runtime.TreeUpdate, 1)
	snapshot := runtime.SessionSnapshot{Session: child}
	updates <- runtime.TreeUpdate{Snapshot: &snapshot}
	close(updates)
	a.consumeTreeObservation(t.Context(), newSeedWatch(), runtime.Observation{TreeUpdates: updates}, 7, false)
	assert.Equal(t, []string{"child-update"}, sessions.lookups)
	require.Len(t, a.events, 1)
	seed, ok := (<-a.events).(*runtime.TokenUsageEvent)
	require.True(t, ok)
	assert.Equal(t, "child-update", seed.SessionID)
}

func newSeedWatch() *treeWatch {
	return &treeWatch{rootSessionID: "root", sessions: map[string]*watchedSession{}}
}

// A child whose lookup exhausts the batch budget must not starve a later live add:
// each live addition gets its own budget, with no lifetime cap.
func TestLateLiveAddIsSeededAfterBatchBudgetElapsed(t *testing.T) {
	usedChild := func(id string) *session.Session {
		sess := session.New(session.WithID(id))
		sess.SetUsage(300, 150)
		return sess
	}
	sessions := &seedSessionsFake{handles: map[string]runtime.SessionHandle{
		"child-1":    &blockingInfoHandle{},
		"child-2":    &agentInfoHandle{info: runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}},
		"child-late": &agentInfoHandle{info: runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}},
	}}
	a := &App{events: make(chan any, 8), descendantSeedBudget: 20 * time.Millisecond, sessions: sessions}
	a.bridgeEpoch.Store(7)
	watch := newSeedWatch()

	a.seedDescendantUsages(t.Context(), watch, []runtime.SessionSnapshot{{Session: usedChild("child-1")}, {Session: usedChild("child-2")}}, 7)
	assert.Equal(t, []string{"child-1"}, sessions.lookups, "the baseline budget stops before child-2")

	a.seedDescendantUsages(t.Context(), watch, []runtime.SessionSnapshot{{Session: usedChild("child-late")}}, 7)
	assert.Equal(t, []string{"child-1", "child-late"}, sessions.lookups)
	require.Len(t, a.events, 1)
	seed, ok := (<-a.events).(*runtime.TokenUsageEvent)
	require.True(t, ok)
	assert.Equal(t, "child-late", seed.SessionID)
}

func TestSeedDescendantUsageSkipsUnchangedUsage(t *testing.T) {
	child := session.New(session.WithID("child"))
	child.SetUsage(300, 150)
	sessions := &seedSessionsFake{handles: map[string]runtime.SessionHandle{
		"child": &agentInfoHandle{info: runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}},
	}}
	a := &App{events: make(chan any, 4), sessions: sessions}
	a.bridgeEpoch.Store(7)
	watch := newSeedWatch()

	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: child}, 7)
	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: child}, 7)
	assert.Equal(t, []string{"child"}, sessions.lookups, "unchanged usage is not looked up again")

	child.SetUsage(400, 150)
	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: child}, 7)
	assert.Equal(t, []string{"child", "child"}, sessions.lookups, "changed usage is seeded again")
	assert.Len(t, a.events, 2)
}

// flakyInfoHandle fails its first agent-info lookup, then answers normally.
type flakyInfoHandle struct {
	runtime.SessionHandle

	calls int
}

func (h *flakyInfoHandle) SessionAgentInfo(context.Context) (runtime.SessionAgentInfo, error) {
	h.calls++
	if h.calls == 1 {
		return runtime.SessionAgentInfo{}, assert.AnError
	}
	return runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}, nil
}

func TestSeedDescendantUsageRetriesAfterFailedLookup(t *testing.T) {
	child := session.New(session.WithID("child"))
	child.SetUsage(300, 150)
	sessions := &seedSessionsFake{handles: map[string]runtime.SessionHandle{"child": &flakyInfoHandle{}}}
	a := &App{events: make(chan any, 4), sessions: sessions}
	a.bridgeEpoch.Store(7)
	watch := newSeedWatch()

	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: child}, 7)
	assert.Empty(t, a.events, "a failed lookup publishes nothing")
	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: child}, 7)
	assert.Equal(t, []string{"child", "child"}, sessions.lookups, "a failed lookup is not recorded as published")
	assert.Len(t, a.events, 1)
}

func TestSeedDescendantUsageDropsOnCanceledDeliveryAndRetries(t *testing.T) {
	child := session.New(session.WithID("child"))
	child.SetUsage(300, 150)
	sessions := &seedSessionsFake{handles: map[string]runtime.SessionHandle{
		"child": &agentInfoHandle{info: runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}},
	}}
	a := &App{events: make(chan any, 1), sessions: sessions}
	a.bridgeEpoch.Store(7)
	watch := newSeedWatch()
	a.events <- "occupied"

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	a.seedDescendantUsage(canceled, watch, runtime.SessionSnapshot{Session: child}, 7)
	assert.Equal(t, "occupied", <-a.events, "the full bus takes no seed while the context is done")

	a.seedDescendantUsage(t.Context(), watch, runtime.SessionSnapshot{Session: child}, 7)
	assert.Equal(t, []string{"child", "child"}, sessions.lookups, "an undelivered seed is not recorded and is looked up again")
	require.Len(t, a.events, 1)
}

// Local roots forward descendant usage without any topology events.
func TestConsumeDescendantUsageSeedsAndForwardsOnlyDescendants(t *testing.T) {
	child := session.New(session.WithID("child"))
	child.SetUsage(300, 150)
	root := session.New(session.WithID("root"))
	root.SetUsage(80, 20)
	sessions := &seedSessionsFake{handles: map[string]runtime.SessionHandle{
		"child": &agentInfoHandle{info: runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: "worker", ContextLimit: 1000}}},
	}}
	a := &App{events: make(chan any, 8), sessions: sessions}
	a.bridgeEpoch.Store(7)
	events := make(chan runtime.SessionEvent, 2)
	events <- runtime.SessionEvent{SessionID: "root", Event: &runtime.TokenUsageEvent{SessionID: "root", Usage: &runtime.Usage{ContextLength: 100}}}
	live := &runtime.TokenUsageEvent{SessionID: "child", Usage: &runtime.Usage{ContextLength: 450, ContextLimit: 1000}}
	events <- runtime.SessionEvent{SessionID: "child", Event: live}
	close(events)

	a.consumeDescendantUsage(t.Context(), newSeedWatch(), runtime.Observation{
		Initial: []runtime.SessionSnapshot{{Session: root}, {Session: child}},
		Events:  events,
	}, 7)

	assert.Equal(t, []string{"child"}, sessions.lookups, "only the descendant is seeded")
	require.Len(t, a.events, 2)
	seed, ok := (<-a.events).(*runtime.TokenUsageEvent)
	require.True(t, ok)
	assert.Equal(t, "child", seed.SessionID)
	assert.Same(t, live, <-a.events, "live descendant usage is forwarded unchanged; root usage is not")
}

type observedHandle struct {
	runtime.SessionHandle

	observe func() (runtime.Observation, error)
	calls   int
}

func (h *observedHandle) Metadata() runtime.SessionMetadata { return runtime.SessionMetadata{} }

func (h *observedHandle) Observe(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
	h.calls++
	return h.observe()
}

type treeRuntime struct {
	Services

	tree *subagent.Tree
}

func (r treeRuntime) SubagentTree() *subagent.Tree { return r.tree }

// One attach starts one descendant observer; closing the bridge context releases it.
func TestDescendantUsageObserverIsReleasedOnContextCancel(t *testing.T) {
	released := make(chan struct{})
	observed := make(chan struct{}, 1)
	handle := &observedHandle{observe: func() (runtime.Observation, error) {
		observed <- struct{}{}
		return runtime.Observation{Events: make(chan runtime.SessionEvent), Cancel: func() { close(released) }}, nil
	}}
	a := &App{events: make(chan any, 1), runtime: treeRuntime{tree: subagent.NewTree()}}
	a.bridgeEpoch.Store(7)
	ctx, cancel := context.WithCancel(t.Context())
	a.startSubagentTreeWatch(ctx, handle, "root", 7)
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("descendant observer never started")
	}
	cancel()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("descendant observer was not released")
	}
}

func TestWatchDescendantUsageRetriesOnlyRetryableErrors(t *testing.T) {
	unsupported := &observedHandle{observe: func() (runtime.Observation, error) {
		return runtime.Observation{}, runtime.UnsupportedSessionOperation("root", "observe_tree")
	}}
	a := &App{events: make(chan any, 1)}
	a.bridgeEpoch.Store(7)
	a.watchDescendantUsage(t.Context(), unsupported, "root", 7)
	assert.Equal(t, 1, unsupported.calls, "unsupported observation is not retried")

	transient := &observedHandle{observe: func() (runtime.Observation, error) {
		return runtime.Observation{}, assert.AnError
	}}
	a.watchDescendantUsage(t.Context(), transient, "root", 7)
	assert.Equal(t, maxTreeRebaselines+1, transient.calls, "transient errors retry at most the rebaseline cap")
}

func TestWatchDescendantUsageCancelsObservationWhenContextEnds(t *testing.T) {
	cancelled := 0
	ctx, cancel := context.WithCancel(t.Context())
	handle := &observedHandle{observe: func() (runtime.Observation, error) {
		cancel()
		return runtime.Observation{Events: make(chan runtime.SessionEvent), Cancel: func() { cancelled++ }}, nil
	}}
	a := &App{events: make(chan any, 1)}
	a.bridgeEpoch.Store(7)
	a.watchDescendantUsage(ctx, handle, "root", 7)
	assert.Equal(t, 1, cancelled, "the live observation is cancelled exactly once when its context ends")
	assert.Equal(t, 1, handle.calls)
}

func TestWatchDescendantUsageDoesNotObserveWithCanceledContext(t *testing.T) {
	handle := &observedHandle{observe: func() (runtime.Observation, error) {
		return runtime.Observation{}, assert.AnError
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a := &App{events: make(chan any, 1)}
	a.bridgeEpoch.Store(7)
	a.watchDescendantUsage(ctx, handle, "root", 7)
	assert.Equal(t, 0, handle.calls, "no observation starts once its context has ended")
}
