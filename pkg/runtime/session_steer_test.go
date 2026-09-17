package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

func newDriverTestRuntime(t *testing.T) *LocalRuntime {
	t.Helper()
	r := &LocalRuntime{
		ctx:           func() context.Context { return context.WithoutCancel(t.Context()) },
		policy:        DefaultSessionResourcePolicy(),
		steerQueue:    NewInMemoryMessageQueue(defaultSteerQueueCapacity),
		followUpQueue: NewInMemoryMessageQueue(defaultFollowUpQueueCapacity),
	}
	r.applyResourcePolicy()
	r.sessionEvents = newSessionEventHubWithLimits(r.maxReplayEvents, r.maxReplayBytes)
	r.sessionDrivers = newSessionDriverRegistry(r)
	return r
}

func TestDetachedSubsessionDoesNotDrainGlobalQueues(t *testing.T) {
	r := newDriverTestRuntime(t)
	require.True(t, r.steerQueue.Enqueue(t.Context(), QueuedMessage{Content: "root steer"}))
	require.True(t, r.followUpQueue.Enqueue(t.Context(), QueuedMessage{Content: "root followup"}))

	child := session.New(
		session.WithID("child"),
		session.WithParentID("root"),
		session.WithNonInteractive(true),
	)
	result := r.drainAndEmitSteered(t.Context(), child, agent.New("worker", ""), NewChannelSink(make(chan Event, 4)))
	assert.False(t, result.drained)

	steered := r.steerQueue.Drain(t.Context())
	require.Len(t, steered, 1)
	assert.Equal(t, "root steer", steered[0].Content)
	followUp, ok := r.followUpQueue.Dequeue(t.Context())
	require.True(t, ok)
	assert.Equal(t, "root followup", followUp.Content)
}

func TestDeliverMessageSteersIntoLiveSessionLoop(t *testing.T) {
	t.Parallel()

	r := newDriverTestRuntime(t)
	sess := session.New(session.WithID("sess"))
	d := r.sessionDrivers.Get(sess)
	d.mu.Lock()
	d.phase = sessionRunning
	d.openSettledLocked()
	d.mu.Unlock()

	assert.True(t, r.sessionDrivers.PostKnown(t.Context(), "sess", QueuedMessage{Content: "note-1"}, false))
	assert.True(t, r.sessionDrivers.PostKnown(t.Context(), "sess", QueuedMessage{Content: "note-2"}, false))

	steered := r.drainSessionSteer("sess")
	require.Len(t, steered, 2)
	assert.Equal(t, "note-1", steered[0].Content)
	assert.Equal(t, "note-2", steered[1].Content)
	assert.Empty(t, r.drainSessionSteer("sess"), "drain consumes the buffer")
}

func TestDeliverMessageRequiresKnownSession(t *testing.T) {
	t.Parallel()

	r := newDriverTestRuntime(t)
	assert.False(t, r.sessionDrivers.PostKnown(t.Context(), "sess", QueuedMessage{Content: "strict"}, false))
	assert.False(t, deliverMessageForTest(r, t.Context(), "sess", "strict"))
}

func TestDeliverOrBufferAdoptsNotesWhenSessionAppears(t *testing.T) {
	t.Parallel()

	r := newDriverTestRuntime(t)
	r.deliverOrBuffer(t.Context(), "sess", "turn-report")
	r.deliverOrBuffer(t.Context(), "sess", "child-message")

	sess := session.New(session.WithID("sess"))
	r.sessionDrivers.Get(sess)
	steered := r.drainSessionSteer("sess")
	require.Len(t, steered, 2)
	assert.Equal(t, "turn-report", steered[0].Content)
	assert.Equal(t, "child-message", steered[1].Content)
}

func TestDeliverMessageWakesKnownIdleSession(t *testing.T) {
	t.Parallel()

	rt, sess := newSessionFixture(t)
	rt.sessionDrivers.Get(sess)
	assert.True(t, deliverMessageForTest(rt, t.Context(), sess.ID, "fresh-turn"))
	assert.Eventually(t, func() bool {
		return rt.sessionDrivers.Settled(sess.ID) && len(sess.Messages) >= 2
	}, 5*time.Second, 10*time.Millisecond)
}

// The runtime derives its subagent store from the session store: SQLite-backed
// session stores persist swarm snapshots in their own table; anything else
// falls back to in-memory. Both are overridable via WithSubagentStore.
func TestSubagentStoreDefaulting(t *testing.T) {
	t.Parallel()

	tm := team.New(team.WithAgents(agent.New("root", "prompt",
		agent.WithModel(&mockProvider{id: "test/mock-model"}))))

	sqlStore, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	defer sqlStore.(*session.SQLiteSessionStore).Close()

	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(sqlStore))
	require.NoError(t, err)
	assert.Same(t, sqlStore, rt.subagentStore, "session store implementing subagent.Store is reused")

	rt, err = NewLocalRuntime(t.Context(), tm)
	require.NoError(t, err)
	assert.IsType(t, &subagent.InMemoryStore{}, rt.subagentStore, "in-memory fallback when the session store cannot persist trees")

	custom := subagent.NewInMemoryStore()
	rt, err = NewLocalRuntime(t.Context(), tm, WithSessionStore(sqlStore), WithSubagentStore(custom))
	require.NoError(t, err)
	assert.Same(t, custom, rt.subagentStore, "explicit WithSubagentStore wins")
}

func TestSessionIdleSteerAcceptsImmediatelyAndDrainsOnNextSend(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	obs, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 32})
	require.NoError(t, err)
	defer obs.Cancel()
	first, err := handle.Steer(t.Context(), TurnInput{Content: "guide one"})
	require.NoError(t, err)
	second, err := handle.Steer(t.Context(), TurnInput{Content: "guide two"})
	require.NoError(t, err)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SessionStateSettled, status.State, "idle steer accepts but does not wake a response")
	seen := map[string]string{}
	for len(seen) < 2 {
		e := <-obs.Events
		if accepted, ok := e.Event.(*PendingUserMessageAcceptedEvent); ok {
			seen[e.TurnID] = accepted.Message
		}
	}
	assert.Equal(t, "guide one", seen[first.TurnID])
	assert.Equal(t, "guide two", seen[second.TurnID])
	_, err = handle.Submit(t.Context(), TurnInput{Content: "start"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return rt.sessionDrivers.Settled(sess.ID) }, 5*time.Second, time.Millisecond)
	items := sessionHandleSnapshot(t, handle).MessagesSnapshot()
	counts := map[string]int{}
	for _, item := range items {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser {
			counts[strings.TrimSpace(item.Message.Message.Content)]++
		}
	}
	assert.Equal(t, 1, counts["guide one"])
	assert.Equal(t, 1, counts["guide two"])
	assert.Equal(t, 1, counts["start"])
}

func TestSteeringDrainWaitsForDurableSessionPromotion(t *testing.T) {
	store := &failingPromotionStore{Store: session.NewInMemorySessionStore()}
	store.fail.Store(true)
	tm := team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/mock-model"}))))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close() })
	sess := session.New(session.WithID("session-sess"))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	d := handle.(*sessionHandle).driver
	d.mu.Lock()
	d.phase = sessionRunning
	d.mu.Unlock()

	require.True(t, d.PostSteer(t.Context(), QueuedMessage{Content: "durable first", RequestID: "turn-1", AcceptedPosition: -1, AcceptedPersisted: true}))
	assert.Empty(t, d.DrainSteering(), "provider must not consume before durable promotion")
	items := sessionHandleSnapshot(t, handle).MessagesSnapshot()
	require.Len(t, items, 1)
	assert.True(t, items[0].Message.Pending)

	store.fail.Store(false)
	promoted := d.DrainSteering()
	require.Len(t, promoted, 1)
	assert.Equal(t, "turn-1", promoted[0].RequestID)
	items = sessionHandleSnapshot(t, handle).MessagesSnapshot()
	require.Len(t, items, 1)
	assert.False(t, items[0].Message.Pending)
}

type failingPromotionStore struct {
	session.Store

	fail atomic.Bool
}

func (s *failingPromotionStore) PromotePendingUserMessage(ctx context.Context, sessionID, turnID string) error {
	if s.fail.Load() {
		return assert.AnError
	}
	return s.Store.PromotePendingUserMessage(ctx, sessionID, turnID)
}

func TestSteeringDrainAtomicSwapLeavesConcurrentArrivalForNextBoundary(t *testing.T) {
	rt, sess := newSessionFixture(t)
	d := rt.sessionDrivers.Get(sess)
	d.phase = sessionRunning
	require.True(t, d.PostSteer(t.Context(), QueuedMessage{Content: "one", RequestID: "one", AcceptedPosition: -1}))
	require.True(t, d.PostSteer(t.Context(), QueuedMessage{Content: "two", RequestID: "two", AcceptedPosition: -1}))
	batch := d.DrainSteering()
	require.True(t, d.PostSteer(t.Context(), QueuedMessage{Content: "three", RequestID: "three", AcceptedPosition: -1}))
	next := d.DrainSteering()
	assert.Equal(t, []string{"one", "two"}, []string{batch[0].Content, batch[1].Content})
	assert.Equal(t, []string{"three"}, []string{next[0].Content})
}
