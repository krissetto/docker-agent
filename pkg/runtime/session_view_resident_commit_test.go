package runtime

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// parkingChildCommitStore parks the first successful child commit after the durable
// write has returned, before completion updates live memory.
type parkingChildCommitStore struct {
	*session.InMemorySessionStore

	childID  string
	parkOnce sync.Once
	parked   chan struct{}
	release  chan struct{}
	relOnce  sync.Once
	loads    atomic.Int64
}

func (s *parkingChildCommitStore) LoadChild(ctx context.Context, id string) (session.ChildRecord, error) {
	s.loads.Add(1)
	return s.InMemorySessionStore.LoadChild(ctx, id)
}

func (s *parkingChildCommitStore) CommitChild(ctx context.Context, c session.ChildCommit) error {
	err := s.InMemorySessionStore.CommitChild(ctx, c)
	if err == nil && c.Record.Node.SessionID == s.childID {
		s.parkOnce.Do(func() { close(s.parked); <-s.release })
	}
	return err
}

func (s *parkingChildCommitStore) CommitChildren(ctx context.Context, commits []session.ChildCommit) error {
	err := s.InMemorySessionStore.CommitChildren(ctx, commits)
	for _, c := range commits {
		if err == nil && c.Record.Node.SessionID == s.childID {
			s.parkOnce.Do(func() { close(s.parked); <-s.release })
		}
	}
	return err
}

func (s *parkingChildCommitStore) unblock() { s.relOnce.Do(func() { close(s.release) }) }

func runSessionToStop(t *testing.T, rt *LocalRuntime, sess *session.Session, binding SessionBinding) {
	t.Helper()
	handle, err := rt.CreateSession(t.Context(), sess, binding)
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 64})
	require.NoError(t, err)
	defer observation.Cancel()
	_, err = handle.Submit(t.Context(), TurnInput{})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for {
		select {
		case ev, ok := <-observation.Events:
			require.True(t, ok, "observation closed before StreamStopped")
			if ev.SessionID != sess.ID {
				continue
			}
			if _, stopped := ev.Event.(*StreamStoppedEvent); stopped {
				return
			}
		case <-ctx.Done():
			t.Fatal("run did not stop within 20s")
		}
	}
}

var residentCommitSeq atomic.Int64

func TestResidentSessionViewPrepareWaitsForParkedChildCommit(t *testing.T) {
	unlimited := DefaultSessionResourcePolicy()
	unlimited.MaxSessions = UnlimitedSessionResources
	cases := []struct {
		name   string
		policy *SessionResourcePolicy
	}{
		{name: "default"},
		{name: "unlimited", policy: &unlimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertPrepareWaitsForParkedChildCommit(t, tc.policy) })
	}
}

func assertPrepareWaitsForParkedChildCommit(t *testing.T, policy *SessionResourcePolicy) {
	t.Helper()
	ctx := t.Context()
	n := residentCommitSeq.Add(1)
	parentID := fmt.Sprintf("resident-parent-%d", n)
	childID := fmt.Sprintf("resident-child-%d", n)
	store := &parkingChildCommitStore{
		InMemorySessionStore: session.NewInMemorySessionStore().(*session.InMemorySessionStore),
		childID:              childID,
		parked:               make(chan struct{}),
		release:              make(chan struct{}),
	}
	prov := &messageRecordingProvider{id: "test/mock-model"}
	root := agent.New("root", "prompt", agent.WithModel(prov), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "planner"}))
	planner := agent.New("planner", "prompt", agent.WithModel(prov))
	opts := []Opt{WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{})}
	if policy != nil {
		opts = append(opts, WithSessionResourcePolicy(*policy))
	}
	rt, err := NewLocalRuntime(ctx, team.New(team.WithAgents(root, planner)), opts...)
	require.NoError(t, err)
	// t.Cleanup runs LIFO: registering shutdown first and unblock second means the
	// parked commit is released before shutdown waits on session work.
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		assert.NoError(t, rt.shutdownSessions(shutdownCtx))
	})
	t.Cleanup(store.unblock)

	runSessionToStop(t, rt, session.New(session.WithID(parentID), session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	runSessionToStop(t, rt, session.New(session.WithID(childID), session.WithParentID(parentID), session.WithAgentName("planner"), session.WithNonInteractive(true)), SessionBinding{AgentName: "planner", ParentSessionID: parentID})

	select {
	case <-store.parked:
	case <-time.After(20 * time.Second):
		t.Fatal("child commit never parked")
	}
	stored, err := store.LoadChild(ctx, childID)
	require.NoError(t, err)
	liveRevision := func() uint64 {
		var rev uint64
		rt.subagents.mu.Lock()
		defer rt.subagents.mu.Unlock()
		for _, rec := range rt.subagents.children {
			if rec.sessionID == childID {
				rev = rec.durable.Revision
			}
		}
		return rev
	}
	t.Logf("child commit parked: store.revision=%d live.revision=%d", stored.Revision, liveRevision())

	view := &localSessionRuntimeView{runtime: rt}
	type outcome struct {
		prepared PreparedSessionView
		err      error
	}
	loadsBeforeProbe := store.loads.Load()
	probeCtx, cancelProbe := context.WithCancel(ctx)
	defer cancelProbe()
	probe := make(chan outcome, 1)
	go func() {
		p, err := view.PrepareSessionView(probeCtx, childID)
		probe <- outcome{p, err}
	}()
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case o := <-probe:
		// The read must wait on the root lock while the commit is parked. Any
		// return here, success or conflict, means it bypassed that lock.
		if o.prepared != nil {
			o.prepared.Abort()
		}
		require.Failf(t, "prepare returned while the child commit was parked", "err=%v", o.err)
	case <-timer.C:
		// Still waiting on the root lifecycle lock; cancel proves it was blocked.
		cancelProbe()
		var o outcome
		select {
		case o = <-probe:
		case <-time.After(20 * time.Second):
			t.Fatal("canceled prepare did not return")
		}
		require.ErrorIs(t, o.err, context.Canceled, "prepare must block on the root lock while parked")
		assert.Equal(t, loadsBeforeProbe, store.loads.Load(), "a blocked prepare must not read child records while parked")
	}

	store.unblock()
	final, err := view.PrepareSessionView(ctx, childID)
	require.NoError(t, err)
	if final != nil {
		final.Abort()
	}
	assert.Equal(t, stored.Revision, liveRevision(), "live revision must catch up after release")
}
