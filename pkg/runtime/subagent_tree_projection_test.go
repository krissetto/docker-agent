package runtime

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

// admitHookStore runs onAdmit once when admission persists the child: spawn has
// passed its parent-state checks then and has not queued the child's tree view yet.
type admitHookStore struct {
	*session.InMemorySessionStore

	once    sync.Once
	onAdmit func()
}

func (s *admitHookStore) AdmitChild(ctx context.Context, admission session.ChildAdmission) error {
	s.once.Do(s.onAdmit)
	return s.InMemorySessionStore.AdmitChild(ctx, admission)
}

// childStates is the child's lifecycle as the parent journal records it:
// creation reports starting, then each tree view that changes its state.
func childStates(events []SequencedSessionEvent, childSessionID string) []string {
	var states []string
	last := ""
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, n := range nodes {
			if n.Node.SessionID == childSessionID && string(n.Node.State) != last {
				last = string(n.Node.State)
				states = append(states, last)
			}
			walk(n.Children)
		}
	}
	for _, envelope := range events {
		switch event := envelope.Event.(type) {
		case *SubagentCreatedEvent:
			if event.ChildSessionID == childSessionID {
				states = append(states, string(subagent.NodeStarting))
			}
		case *SubagentTreeEvent:
			walk(event.Snapshot.Nodes)
		}
	}
	return states
}

// blockingWorker enters its first model call, then waits for release.
func blockingWorker() (provider *coordinationProvider, entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	provider = coordinationReply("child done")
	provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return newStreamBuilder().AddContent("child done").AddStopWithUsage(1, 1).Build(), nil
	}
	return provider, entered, release
}

// holdOwner occupies the driver's owner loop (which holds the driver lock while
// it runs a command) until the returned release is closed, so no queued tree view
// can be published asynchronously meanwhile. It returns once the owner is held.
func holdOwner(ctx context.Context, d *sessionDriver) (release chan struct{}) {
	entered, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = d.ownerCall(ctx, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	return release
}

// takeHeld returns the owner release that admission installed. releaseHeld frees
// a still-held owner when a test fails before releasing it.
func takeHeld(held chan chan struct{}) chan struct{} {
	release := <-held
	held <- nil
	return release
}

func releaseHeld(held chan chan struct{}) {
	select {
	case release := <-held:
		if release != nil {
			close(release)
		}
	default:
	}
}

// journal replays the parent's journal without taking the driver lock, which a
// held owner keeps.
func journal(d *sessionDriver, sessionID string) []SequencedSessionEvent {
	zero := uint64(0)
	replay, _, cancel, _ := d.events.SubscribeSequenced(sessionID, &zero, 128)
	cancel()
	return replay
}

func spawnedChild(t *testing.T, events []SequencedSessionEvent) string {
	t.Helper()
	for _, envelope := range events {
		if created, ok := envelope.Event.(*SubagentCreatedEvent); ok {
			return created.ChildSessionID
		}
	}
	t.Fatal("no subagent was created")
	return ""
}

func awaitChildIdle(t *testing.T, d *sessionDriver) string {
	t.Helper()
	var child string
	require.Eventually(t, func() bool {
		events := canonicalReplay(d)
		for _, envelope := range events {
			if created, ok := envelope.Event.(*SubagentCreatedEvent); ok {
				child = created.ChildSessionID
			}
		}
		states := childStates(events, child)
		return child != "" && len(states) > 1 && states[len(states)-1] == string(subagent.NodeIdle) && slices.Contains(states, string(subagent.NodeRunning))
	}, 10*time.Second, time.Millisecond)
	return child
}

// The view admission queues shows the child idle. Even when the parent's owner is
// busy and could only publish it later, it reaches the journal before the child
// runs: a running view never replaces it unseen.
func TestSpawnPublishesCreationIdleViewBeforeChildRuns(t *testing.T) {
	worker, childEntered, childRelease := blockingWorker()
	store := &admitHookStore{InMemorySessionStore: session.NewInMemorySessionStore().(*session.InMemorySessionStore)}
	rt, owner := coordinationRuntime(t, store, coordinationReply("received"), worker)
	parent := coordinationCreate(t, owner.Runtime(), "parent", "")
	d := parent.(*sessionHandle).driver
	parentSession := d.session()
	allowed := rt.allowedFromAgent(rt.resolveSessionAgent(parentSession))
	require.Len(t, allowed, 1)

	// From admission on, the parent's owner is busy and cannot publish queued views.
	held := make(chan chan struct{}, 1)
	store.onAdmit = func() { held <- holdOwner(t.Context(), d) }
	t.Cleanup(func() { releaseHeld(held) })
	spawned := make(chan error, 1)
	go func() {
		_, err := rt.subagents.Spawn(parentSession, "root", allowed[0], "work")
		spawned <- err
	}()
	// Release the owner once spawn waits on it, or once spawn has already started
	// the child without publishing the idle view.
	require.Eventually(t, func() bool { return len(spawned) > 0 || len(d.ownerCommands) > 0 }, 10*time.Second, time.Millisecond)
	close(takeHeld(held))
	coordinationWait(t, childEntered)
	close(childRelease)
	select {
	case err := <-spawned:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not return")
	}

	child := awaitChildIdle(t, d)
	assert.Equal(t, []string{"starting", "idle", "running", "idle"}, childStates(canonicalReplay(d), child))
}

// If publishing the admitted child's view fails (here the spawn is canceled while
// the owner is busy), the child still starts and the owner publishes the newest
// view asynchronously once it is free.
func TestSpawnStartsChildWhenCreationViewCannotBePublished(t *testing.T) {
	worker, childEntered, childRelease := blockingWorker()
	store := &admitHookStore{InMemorySessionStore: session.NewInMemorySessionStore().(*session.InMemorySessionStore)}
	rt, owner := coordinationRuntime(t, store, coordinationReply("received"), worker)
	parent := coordinationCreate(t, owner.Runtime(), "parent", "")
	d := parent.(*sessionHandle).driver
	parentSession := d.session()
	allowed := rt.allowedFromAgent(rt.resolveSessionAgent(parentSession))
	require.Len(t, allowed, 1)

	held := make(chan chan struct{}, 1)
	store.onAdmit = func() { held <- holdOwner(t.Context(), d) }
	t.Cleanup(func() { releaseHeld(held) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	spawned := make(chan error, 1)
	go func() {
		_, err := rt.subagents.spawnContext(ctx, parentSession, "root", allowed[0], "work")
		spawned <- err
	}()
	// Spawn waits for the busy owner; cancel it there.
	require.Eventually(t, func() bool { return len(d.ownerCommands) > 0 }, 10*time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-spawned:
		require.NoError(t, err, "admission had committed; only the synchronous publish was abandoned")
	case <-time.After(10 * time.Second):
		t.Fatal("spawn did not return after cancellation")
	}
	events := journal(d, parent.ID())
	childID := spawnedChild(t, events)
	cd, ok := rt.sessionDrivers.Lookup(childID)
	require.True(t, ok)
	cd.mu.Lock()
	started := cd.running() || cd.starting()
	cd.mu.Unlock()
	assert.True(t, started, "the child was woken although its view could not be published")
	assert.Equal(t, []string{"starting"}, childStates(events, childID), "no view is published while the owner is busy")

	close(takeHeld(held))
	coordinationWait(t, childEntered)
	close(childRelease)
	child := awaitChildIdle(t, d)
	states := childStates(canonicalReplay(d), child)
	assert.Equal(t, "starting", states[0])
	assert.Equal(t, "idle", states[len(states)-1])
}
