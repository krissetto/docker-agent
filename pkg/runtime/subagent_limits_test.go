package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestLimitAllows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		n, limit   int
		wantAllows bool
	}{
		{name: "negative unlimited", n: 1_000, limit: -1, wantAllows: true},
		{name: "zero denies", n: 0, limit: 0, wantAllows: false},
		{name: "positive below", n: 2, limit: 3, wantAllows: true},
		{name: "positive at limit", n: 3, limit: 3, wantAllows: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantAllows, limitAllows(tc.n, tc.limit))
		})
	}
}

func TestChildAdmissionReturnsTypedDenialReason(t *testing.T) {
	m := newTestSubagentManager(t)
	m.r.maxActiveDescendants = 0
	m.r.maxActiveDescendantsRoot = -1
	m.r.maxSubagentDepth = -1
	decision := m.childAdmissionLocked(childAdmissionRequest{parentSession: "root", spawn: true})
	assert.Equal(t, childAdmissionGlobalActive, decision.denial)
	assert.Equal(t, 0, decision.limit)
}

func TestSubagentAdmissionDepthAndCount(t *testing.T) {
	m := newTestSubagentManager(t)
	m.r.maxActiveDescendants = 2
	m.r.maxActiveDescendantsRoot = 2
	m.r.maxSubagentDepth = 2
	root := session.New(session.WithID("root"))
	child := session.New(session.WithID("child"))
	m.registerChild(root, "root", "aaaaa", "worker", child)
	require.NoError(t, m.spawnAdmissionErrorLocked(child))
	grandchild := session.New(session.WithID("grandchild"))
	m.registerChild(child, "worker", "bbbbb", "worker", grandchild)
	require.ErrorContains(t, m.spawnAdmissionErrorLocked(root), "limit reached")
	require.ErrorContains(t, m.spawnAdmissionErrorLocked(grandchild), "limit")
}

type capacityBlockingProvider struct {
	*mockProvider

	entered chan struct{}
	current atomic.Int64
	max     atomic.Int64
}

func (p *capacityBlockingProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	current := p.current.Add(1)
	defer p.current.Add(-1)
	for {
		maximum := p.max.Load()
		if current <= maximum || p.max.CompareAndSwap(maximum, current) {
			break
		}
	}
	p.entered <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestActualSpawnCapacityAndRelease(t *testing.T) {
	workerProvider := &capacityBlockingProvider{
		mockProvider: &mockProvider{id: "test/blocking-worker"},
		entered:      make(chan struct{}, 101),
	}
	rootProvider := &multiRunProvider{mockProvider: &mockProvider{id: "test/root"}, build: func() chat.MessageStream {
		return newStreamBuilder().AddContent("title").AddStopWithUsage(1, 1).Build()
	}}
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(rootProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(workerProvider)),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithMaxActiveDescendants(100), WithMaxActiveDescendantsPerRoot(100))
	require.NoError(t, err)
	parent := session.New(session.WithID("capacity-root"))
	ref := subagent.AllowedSubagent{Agent: "worker"}
	ids := make([]subagent.NodeID, 100)
	for i := range ids {
		ids[i], err = rt.subagents.Spawn(parent, "root", ref, fmt.Sprintf("block-%d", i))
		require.NoError(t, err)
	}
	for range 100 {
		select {
		case <-workerProvider.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("100 worker provider calls did not enter")
		}
	}
	assert.Equal(t, int64(100), workerProvider.current.Load())
	assert.Equal(t, int64(100), workerProvider.max.Load())
	select {
	case <-workerProvider.entered:
		t.Fatal("more than 100 worker provider calls entered")
	default:
	}
	_, err = rt.subagents.Spawn(parent, "root", ref, "denied")
	require.ErrorContains(t, err, "100")

	_, err = rt.subagents.stopChild(parent.ID, ids[0])
	require.NoError(t, err)
	_, err = rt.subagents.Spawn(parent, "root", ref, "replacement")
	require.NoError(t, err)
	select {
	case <-workerProvider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not enter worker provider after capacity release")
	}
	assert.Equal(t, int64(100), workerProvider.current.Load())
	assert.Equal(t, int64(100), workerProvider.max.Load())
	require.NoError(t, rt.Close())
}

func TestActualSequentialSpawnSettleCyclesDoNotExhaust(t *testing.T) {
	provider := &multiRunProvider{
		mockProvider: &mockProvider{id: "test/quick-worker"},
		build: func() chat.MessageStream {
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build()
		},
	}
	rootProvider := &multiRunProvider{mockProvider: &mockProvider{id: "test/root"}, build: func() chat.MessageStream {
		return newStreamBuilder().AddContent("title").AddStopWithUsage(1, 1).Build()
	}}
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(rootProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(provider)),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithMaxActiveDescendants(3), WithMaxActiveDescendantsPerRoot(3))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	parent := session.New(session.WithID("sequential-root"))
	ref := subagent.AllowedSubagent{Agent: "worker"}
	for i := range 150 {
		id, spawnErr := rt.subagents.Spawn(parent, "root", ref, fmt.Sprintf("turn-%d", i))
		require.NoError(t, spawnErr)
		require.Eventually(t, func() bool {
			rec, ok := rt.subagents.Read(id)
			return ok && (rec.state == subagent.NodeIdle || rec.state == subagent.NodeFailed)
		}, 2*time.Second, time.Millisecond, "cycle %d did not settle", i)
	}
	assert.Len(t, rt.subagents.children, 150)
}

func TestNestedGlobalAndPerRootRunLimits(t *testing.T) {
	m := newTestSubagentManager(t)
	m.r.maxActiveDescendants = 3
	m.r.maxActiveDescendantsRoot = 2
	rootA := session.New(session.WithID("root-a"))
	rootB := session.New(session.WithID("root-b"))
	childA := session.New(session.WithID("child-a"))
	m.registerChild(rootA, "root", "a0001", "worker", childA)
	m.registerChild(childA, "worker", "a0002", "worker", session.New(session.WithID("grand-a")))
	m.registerChild(rootA, "root", "a0003", "worker", session.New(session.WithID("idle-a")))
	m.children["a0003"].state = subagent.NodeIdle
	require.Error(t, m.admitChildRun("a0003"), "nested runs share their root's limit")

	m.registerChild(rootB, "root", "b0001", "worker", session.New(session.WithID("child-b")))
	m.registerChild(rootB, "root", "b0002", "worker", session.New(session.WithID("idle-b")))
	m.children["b0002"].state = subagent.NodeIdle
	require.Error(t, m.admitChildRun("b0002"), "global limit spans independent roots")

	m.children["a0002"].state = subagent.NodeIdle
	assert.NoError(t, m.admitChildRun("b0002"), "a nested settlement releases global capacity")
}

func TestFailedSessionMessageableButRunGated(t *testing.T) {
	m := newTestSubagentManager(t)
	m.r.maxActiveDescendants = 1
	m.r.maxActiveDescendantsRoot = 1
	parent := session.New(session.WithID("root"))
	failed := session.New(session.WithID("failed"))
	active := session.New(session.WithID("active"))
	m.registerChild(parent, "root", "f0001", "worker", failed)
	m.children["f0001"].state = subagent.NodeFailed
	failedDriver := m.r.sessionDrivers.Get(failed)
	failedDriver.mu.Lock()
	failedDriver.running = false
	failedDriver.closeSettledLocked()
	failedDriver.mu.Unlock()
	failedDriver.SetPreStartErrorGate(func() error { return m.admitChildRun("f0001") }, func() { m.abortChildStart("f0001") })
	m.registerChild(parent, "root", "a0001", "worker", active)

	_, err := m.sendToChild(parent.ID, "f0001", "retry")
	require.NoError(t, err, "durable acceptance remains successful even when execution admission is denied")
	assert.Equal(t, subagent.NodeFailed, m.children["f0001"].state)
	assert.True(t, m.r.sessionDrivers.Get(failed).HasPending())
}

func TestProviderErrorReleasesRunCapacity(t *testing.T) {
	rootProvider := &multiRunProvider{mockProvider: &mockProvider{id: "test/root"}, build: func() chat.MessageStream {
		return newStreamBuilder().AddContent("title").AddStopWithUsage(1, 1).Build()
	}}
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(rootProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(&mockProviderWithError{id: "test/failing-worker"})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithMaxActiveDescendants(1), WithMaxActiveDescendantsPerRoot(1))
	require.NoError(t, err)
	defer func() { require.NoError(t, rt.Close()) }()
	parent := session.New(session.WithID("provider-error-root"))
	ref := subagent.AllowedSubagent{Agent: "worker"}
	first, err := rt.subagents.Spawn(parent, "root", ref, "fail once")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		rec, ok := rt.subagents.Read(first)
		return ok && rec.state == subagent.NodeFailed
	}, 2*time.Second, time.Millisecond)
	_, err = rt.subagents.Spawn(parent, "root", ref, "capacity reused")
	require.NoError(t, err)
}

func TestSubagentAdmissionCountsOnlyStartingAndRunning(t *testing.T) {
	m := newTestSubagentManager(t)
	m.r.maxActiveDescendants = 2
	m.r.maxActiveDescendantsRoot = 2
	m.r.maxSubagentDepth = 3
	root := session.New(session.WithID("root"))

	states := []subagent.NodeState{subagent.NodeIdle, subagent.NodeFailed, subagent.NodeCompleted, subagent.NodeStopped}
	for i, state := range states {
		child := session.New(session.WithID(fmt.Sprintf("settled-%d", i)))
		m.registerChild(root, "root", subagent.NodeID(fmt.Sprintf("s%04d", i)), "worker", child)
		m.children[subagent.NodeID(fmt.Sprintf("s%04d", i))].state = state
	}
	require.NoError(t, m.spawnAdmissionErrorLocked(root), "retained quiescent sessions do not consume run capacity")

	m.children["s0000"].state = subagent.NodeStarting
	m.children["s0001"].state = subagent.NodeRunning
	err := m.spawnAdmissionErrorLocked(root)
	require.ErrorContains(t, err, "active/running")

	m.children["s0001"].state = subagent.NodeIdle
	require.NoError(t, m.spawnAdmissionErrorLocked(root), "settling releases exactly one slot")
}

func TestSequentialSettledSubagentsNeverExhaustActiveCapacity(t *testing.T) {
	m := newTestSubagentManager(t)
	m.r.maxActiveDescendants = 100
	m.r.maxActiveDescendantsRoot = 100
	m.r.maxSubagentDepth = 3
	root := session.New(session.WithID("root"))

	for i := range 200 {
		id := subagent.NodeID(fmt.Sprintf("q%04d", i))
		child := session.New(session.WithID(fmt.Sprintf("child-%d", i)))
		m.registerChild(root, "root", id, "worker", child)
		m.children[id].state = subagent.NodeIdle
		require.NoError(t, m.spawnAdmissionErrorLocked(root), "settled retained session %d must not consume capacity", i)
	}
	assert.Len(t, m.children, 200)
}

func TestAccountedActiveMatchesTreeVisibleState(t *testing.T) {
	m := newTestSubagentManager(t)
	m.r.maxActiveDescendants = 100
	m.r.maxActiveDescendantsRoot = 100
	root := session.New(session.WithID("root"))
	states := []subagent.NodeState{subagent.NodeStarting, subagent.NodeRunning, subagent.NodeIdle, subagent.NodeFailed, subagent.NodeCompleted, subagent.NodeStopped}
	for i, state := range states {
		id := subagent.NodeID(fmt.Sprintf("v%04d", i))
		m.registerChild(root, "root", id, "worker", session.New(session.WithID(fmt.Sprintf("visible-%d", i))))
		m.children[id].state = state
		require.NoError(t, m.tree.Update(id, func(n *subagent.Node) { n.State = state }))
	}

	accounted := 0
	for _, rec := range m.children {
		if activeSubagentState(rec.state) {
			accounted++
		}
	}
	visible := 0
	var visit func([]subagent.NodeSnapshot)
	visit = func(nodes []subagent.NodeSnapshot) {
		for _, snap := range nodes {
			if snap.Node.State == subagent.NodeStarting || snap.Node.State == subagent.NodeRunning {
				visible++
			}
			visit(snap.Children)
		}
	}
	visit(m.tree.Snapshot().Nodes)
	assert.Equal(t, 2, accounted)
	assert.Equal(t, accounted+1, visible, "tree also includes the synthetic running root")
}

func TestSubagentRunAdmissionIsAtomicAndSharedAcrossRoots(t *testing.T) {
	m := newTestSubagentManager(t)
	m.r.maxActiveDescendants = 100
	m.r.maxActiveDescendantsRoot = 100
	m.r.maxSubagentDepth = 3
	roots := []*session.Session{session.New(session.WithID("root-a")), session.New(session.WithID("root-b"))}

	ids := make([]subagent.NodeID, 101)
	for i := range ids {
		id := subagent.NodeID(fmt.Sprintf("c%04d", i))
		ids[i] = id
		root := roots[i%len(roots)]
		child := session.New(session.WithID(fmt.Sprintf("child-%d", i)))
		m.registerChild(root, "root", id, "worker", child)
		m.children[id].state = subagent.NodeIdle
	}

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			if m.admitChildRun(id) == nil {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int64(100), accepted.Load())

	active := 0
	var denied subagent.NodeID
	m.mu.Lock()
	for id, rec := range m.children {
		if activeSubagentState(rec.state) {
			active++
		} else {
			denied = id
		}
	}
	m.mu.Unlock()
	assert.Equal(t, 100, active)
	require.NotEmpty(t, denied)

	m.mu.Lock()
	for id, rec := range m.children {
		if activeSubagentState(rec.state) {
			rec.state = subagent.NodeIdle
			_ = m.tree.Update(id, func(n *subagent.Node) { n.State = subagent.NodeIdle })
			break
		}
	}
	m.mu.Unlock()
	assert.NoError(t, m.admitChildRun(denied), "released capacity is immediately reusable")
}

func TestConcurrentPostsWaitForAdmissionAndPreserveOrder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		firstGate bool
		wantFirst bool
	}{
		{name: "commit", firstGate: true, wantFirst: true},
		{name: "denied first retains both", firstGate: false, wantFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, sess := newSessionFixture(t)
			d := r.sessionDrivers.Get(sess)
			entered := make(chan struct{})
			release := make(chan struct{})
			var gates atomic.Int64
			d.SetPreStartGate(func() bool {
				call := gates.Add(1)
				if call == 1 {
					close(entered)
					<-release
					return tc.firstGate
				}
				return true
			}, nil)

			first := make(chan bool, 1)
			second := make(chan bool, 1)
			go func() { first <- d.Post(t.Context(), QueuedMessage{Content: "first"}, true) }()
			<-entered
			go func() { second <- d.Post(t.Context(), QueuedMessage{Content: "second"}, true) }()
			require.Never(t, func() bool {
				select {
				case <-second:
					return true
				default:
					return false
				}
			}, 20*time.Millisecond, time.Millisecond, "second post must wait for the first handshake")
			close(release)

			assert.Equal(t, tc.wantFirst, <-first)
			assert.True(t, <-second, "second post is accepted behind a denied predecessor")
			require.Eventually(t, func() bool {
				var seen []string
				for _, item := range sess.GetAllMessages() {
					if item.Message.Role == chat.MessageRoleUser {
						seen = append(seen, item.Message.Content)
					}
				}
				if tc.firstGate {
					return len(seen) >= 2 && strings.TrimSpace(seen[0]) == "first" && seen[1] == "second"
				}
				return d.HasPending()
			}, 2*time.Second, time.Millisecond)
			if tc.firstGate {
				assert.Equal(t, int64(1), gates.Load())
			} else {
				assert.Equal(t, int64(2), gates.Load(), "a later accepted post may retry waking the retained FIFO head")
			}
		})
	}
}

func TestPostWaitingForAdmissionHonorsContextAndStop(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID("child")))
	entered := make(chan struct{})
	release := make(chan struct{})
	d.SetPreStartGate(func() bool { close(entered); <-release; return true }, nil)
	first := make(chan bool, 1)
	go func() { first <- d.Post(t.Context(), QueuedMessage{Content: "first"}, true) }()
	<-entered

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.False(t, d.Post(ctx, QueuedMessage{Content: "cancelled"}, true))
	d.StopAll()
	assert.False(t, d.Post(t.Context(), QueuedMessage{Content: "stopped"}, true))
	close(release)
	assert.False(t, <-first)
}

func TestDeniedIdleWakeDoesNotStrandMessageOrMarkRunning(t *testing.T) {
	r := newDriverTestRuntime(t)
	sess := session.New(session.WithID("child"), session.WithParentID("root"))
	d := r.sessionDrivers.Get(sess)
	d.SetPreStartGate(func() bool { return false }, nil)

	assert.True(t, d.Post(t.Context(), QueuedMessage{Content: "denied", RequestID: "accepted-b"}, true), "accepted B remains a successful submission when waking A fails")
	assert.True(t, d.HasPending(), "accepted input remains queued for a later wake")
	d.mu.Lock()
	defer d.mu.Unlock()
	assert.False(t, d.running)
	assert.False(t, d.starting)
}

func TestDeniedIdleWakePreservesUnrelatedQueuedMessages(t *testing.T) {
	r := newDriverTestRuntime(t)
	sess := session.New(session.WithID("child"), session.WithParentID("root"))
	d := r.sessionDrivers.Get(sess)
	d.mu.Lock()
	d.pending = append(d.pending, QueuedMessage{Content: "unrelated"})
	d.mu.Unlock()
	d.SetPreStartGate(func() bool { return false }, nil)

	assert.True(t, d.Post(t.Context(), QueuedMessage{Content: "denied", RequestID: "accepted-b"}, true), "accepted B remains successful when waking A fails")
	pending := d.DrainPending()
	require.Len(t, pending, 2)
	assert.Equal(t, []string{"unrelated", "denied"}, []string{pending[0].Content, pending[1].Content})
}

func TestContinuousPendingTurnKeepsReservationAndSettlesOnce(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID("child"), session.WithParentID("root")))
	var starts, settles atomic.Int64
	d.SetPreStartGate(func() bool { return true }, nil)
	d.OnStarted(func() { starts.Add(1) })
	d.OnSettled(func() { settles.Add(1) })

	_, generation, ok := d.tryStart(t.Context())
	require.True(t, ok)
	require.True(t, d.Post(t.Context(), QueuedMessage{Content: "next", RequestID: "next"}, false))
	_, generation, again := d.finishRun(generation, "")
	assert.True(t, again, "accepted input continues in the same logical run")
	assert.Equal(t, int64(1), starts.Load())
	assert.Equal(t, int64(0), settles.Load(), "no false idle report between turns")
	assert.False(t, d.Settled())

	require.Empty(t, d.DrainPending(), "promoted normal input leaves the FIFO exactly once")
	_, _, again = d.finishRun(generation, "")
	assert.False(t, again)
	assert.Equal(t, int64(1), starts.Load(), "continuous turn is not re-admitted")
	assert.Equal(t, int64(1), settles.Load())
	assert.True(t, d.Settled())
	d.wg.Done()
}

func TestStopAllWinsInFlightStartAndAbortsReservationOnce(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID("child"), session.WithParentID("root")))
	entered := make(chan struct{})
	release := make(chan struct{})
	var aborts atomic.Int64
	d.SetPreStartGate(func() bool {
		close(entered)
		<-release
		return true
	}, func() { aborts.Add(1) })

	done := make(chan bool, 1)
	go func() {
		_, _, ok := d.tryStart(t.Context())
		done <- ok
	}()
	<-entered
	assert.False(t, d.Settled(), "starting is not settled")
	assert.False(t, d.StopAll(), "no running cancellation exists yet")
	close(release)
	select {
	case ok := <-done:
		assert.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("start did not resolve after gate release")
	}
	assert.Equal(t, int64(1), aborts.Load())
	assert.True(t, d.Settled())
}

func TestReliablePostRetainsDeniedNotesAndLaterProcessesFIFO(t *testing.T) {
	r, sess := newSessionFixture(t)
	d := r.sessionDrivers.Get(sess)
	var admit atomic.Bool
	d.SetPreStartGate(admit.Load, nil)

	require.True(t, d.PostReliable(t.Context(), QueuedMessage{Content: "first"}))
	require.True(t, d.PostReliable(t.Context(), QueuedMessage{Content: "second"}))
	assert.True(t, d.HasPending(), "admission denial retains accepted notes")
	d.mu.Lock()
	assert.False(t, d.running)
	d.mu.Unlock()

	admit.Store(true)
	require.True(t, d.WakePending())
	require.Eventually(t, func() bool {
		var users []string
		for _, item := range sess.GetAllMessages() {
			if item.Message.Role == chat.MessageRoleUser {
				users = append(users, strings.TrimSpace(item.Message.Content))
			}
		}
		return len(users) >= 2 && users[0] == "first" && users[1] == "second"
	}, 2*time.Second, time.Millisecond)
}

func TestReliablePostMailboxBound(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.maxPendingMailbox = 2
	d := r.sessionDrivers.Get(session.New(session.WithID("reliable-child"), session.WithParentID("root")))
	d.SetPreStartGate(func() bool { return false }, nil)

	assert.True(t, d.PostReliable(t.Context(), QueuedMessage{Content: "one"}))
	assert.True(t, d.PostReliable(t.Context(), QueuedMessage{Content: "two"}))
	assert.False(t, d.PostReliable(t.Context(), QueuedMessage{Content: "overflow"}))
	pending := d.DrainPending()
	require.Len(t, pending, 2)
	assert.Equal(t, "one", pending[0].Content)
	assert.Equal(t, "two", pending[1].Content)
}

func TestCapacityReleaseWorkerWakesRetainedReliableNote(t *testing.T) {
	r, waiting := newSessionFixture(t)
	m := r.subagents
	m.r.maxActiveDescendants = 1
	m.r.maxActiveDescendantsRoot = 1
	root := session.New(session.WithID("root"))
	active := session.New(session.WithID("active"))
	m.registerChild(root, "root", "active", "root", active)
	m.registerChild(root, "root", "waiting", "root", waiting)

	m.mu.Lock()
	m.children["waiting"].state = subagent.NodeIdle
	_ = m.tree.Update("waiting", func(n *subagent.Node) { n.State = subagent.NodeIdle })
	m.mu.Unlock()
	waitingDriver := m.r.sessionDrivers.Get(waiting)
	waitingDriver.mu.Lock()
	waitingDriver.running = false
	waitingDriver.closeSettledLocked()
	waitingDriver.mu.Unlock()
	require.True(t, waitingDriver.PostReliable(t.Context(), QueuedMessage{Content: "later"}))
	assert.True(t, waitingDriver.HasPending())

	m.mu.Lock()
	m.children["active"].state = subagent.NodeIdle
	_ = m.tree.Update("active", func(n *subagent.Node) { n.State = subagent.NodeIdle })
	m.mu.Unlock()
	m.signalCapacityRelease()
	require.Eventually(t, func() bool {
		var sawLater, sawReply bool
		for _, item := range waiting.GetAllMessages() {
			switch item.Message.Role {
			case chat.MessageRoleUser:
				sawLater = sawLater || strings.TrimSpace(item.Message.Content) == "later"
			case chat.MessageRoleAssistant:
				sawReply = sawReply || strings.TrimSpace(item.Message.Content) == "ok"
			}
		}
		rec, ok := m.Read("waiting")
		return sawLater && sawReply && ok && rec.state == subagent.NodeIdle && rec.result == "ok" && waitingDriver.Settled()
	}, 2*time.Second, time.Millisecond,
		"capacity release must wake the retained note through a complete, durably observable turn")
	require.Eventually(t, func() bool {
		m.r.sessionDrivers.mu.Lock()
		defer m.r.sessionDrivers.mu.Unlock()
		for _, msg := range m.r.sessionDrivers.orphans[root.ID] {
			if strings.Contains(msg.Content, "finished its turn") && strings.Contains(msg.Content, "ok") {
				return true
			}
		}
		return false
	}, 2*time.Second, time.Millisecond,
		"the completed wake turn must retain its report for the not-yet-adopted parent")
}

func TestSessionDriverMailboxBound(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.maxPendingMailbox = 2
	s := session.New(session.WithID("s"))
	d := r.sessionDrivers.Get(s)
	d.mu.Lock()
	d.running = true
	d.openSettledLocked()
	d.mu.Unlock()
	assert.True(t, d.Post(t.Context(), QueuedMessage{Content: "1"}, false))
	assert.True(t, d.Post(t.Context(), QueuedMessage{Content: "2"}, false))
	assert.False(t, d.Post(t.Context(), QueuedMessage{Content: "3"}, false))
}

func TestOrphanMailboxBound(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.maxOrphanMailbox = 2
	assert.True(t, r.sessionDrivers.PostOrBuffer(t.Context(), "s", QueuedMessage{Content: "1"}, true))
	assert.True(t, r.sessionDrivers.PostOrBuffer(t.Context(), "s", QueuedMessage{Content: "2"}, true))
	assert.False(t, r.sessionDrivers.PostOrBuffer(t.Context(), "s", QueuedMessage{Content: "3"}, true))
}

func TestFullOrphanMailboxIsAdoptedBeforeDriverPublication(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.maxOrphanMailbox = 2
	r.maxPendingMailbox = 2
	g := r.sessionDrivers
	require.True(t, g.PostReliable(t.Context(), "s", QueuedMessage{Content: "old-1"}))
	require.True(t, g.PostReliable(t.Context(), "s", QueuedMessage{Content: "old-2"}))

	d := g.Get(session.New(session.WithID("s")))
	assert.False(t, g.PostReliable(t.Context(), "s", QueuedMessage{Content: "new"}),
		"new delivery observes the already-full adopted mailbox")
	pending := d.DrainPending()
	require.Len(t, pending, 2)
	assert.Equal(t, "old-1", pending[0].Content)
	assert.Equal(t, "old-2", pending[1].Content)
}

func TestAsyncChildGetsParentMessageTool(t *testing.T) {
	s := session.New(session.WithAsyncSubagent(true))
	childTools := addAsyncChildTools(s, nil)
	require.Len(t, childTools, 1)
	assert.Equal(t, subagent.ToolSendMessage, childTools[0].Name)
	root := session.New()
	assert.Empty(t, addAsyncChildTools(root, nil))
}
