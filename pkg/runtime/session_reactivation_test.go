package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
)

type reactivationAckStore struct {
	*session.SQLiteSessionStore
	loseAck bool
}

func (s *reactivationAckStore) ReactivateChildWithInput(ctx context.Context, record session.ChildRecord, input *session.Message) (session.ChildRecord, *session.Message, error) {
	record, accepted, err := s.SQLiteSessionStore.ReactivateChildWithInput(ctx, record, input)
	if err == nil && s.loseAck {
		return session.ChildRecord{}, nil, errors.New("synthetic lost acknowledgement")
	}
	return record, accepted, err
}

func stoppedViewRuntime(t *testing.T, store session.Store, calls *atomic.Int32) (*LocalRuntime, SessionRuntimeSupervisor) {
	t.Helper()
	model := coordinationReply("done")
	model.call = func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
		calls.Add(1)
		for _, message := range messages {
			assert.NotContains(t, message.Content, "obsolete pending")
		}
		return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
	}
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(model), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(model), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	return rt, owner
}

func stoppedViewFixture(t *testing.T, store session.Store) {
	t.Helper()
	root := session.New(session.WithID("root"), session.WithAttributes(map[string]string{SessionAgentAttribute: "root"}))
	root.AgentName = "root"
	require.NoError(t, store.AddSession(t.Context(), root))
	parent, parentNode, parentAgent := "root", subagent.SessionRootID("root"), "root"
	for _, id := range []string{"parent", "target", "descendant"} {
		child := session.New(session.WithID(id), session.WithParentID(parent), session.WithAttributes(map[string]string{SessionAgentAttribute: "worker", SessionParentAgentAttribute: parentAgent}))
		child.AgentName = "worker"
		child.AddMessage(session.UserMessage("retained history " + id))
		pending := session.UserMessage("obsolete pending " + id)
		pending.TurnID, _ = sessionInputID(id, "old")
		pending.InputOrigin, pending.InputMode, pending.Pending, pending.Accepted = session.InputOriginUser, "turn", true, true
		child.AddMessage(pending)
		state := subagent.NodeStopped
		if id == "descendant" {
			state = subagent.NodeIdle
		}
		record := session.ChildRecord{RootSessionID: "root", ParentSessionID: parent, Node: subagent.Node{ID: subagent.NodeID(id), SessionID: id, Parent: parentNode, Agent: "worker", State: state}}
		require.NoError(t, store.(session.CoordinationStore).AdmitChild(t.Context(), session.ChildAdmission{Child: child, Record: record}))
		if id == "descendant" {
			record.Revision = 1
			require.NoError(t, store.(session.CoordinationStore).CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: 1, Record: record, Reports: []session.ChildReport{{ID: "obsolete-report", ParentSessionID: parent, ChildSessionID: id, TurnID: "old-result", Content: "obsolete pending report"}}}))
			record.Revision, record.Node.State = 2, subagent.NodeStopped
			require.NoError(t, store.(session.CoordinationStore).CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: 2, Record: record}))
		}
		parent, parentNode, parentAgent = id, subagent.NodeID(id), "worker"
	}
}

func openStoppedView(t *testing.T, owner SessionRuntimeSupervisor, id string) SessionHandle {
	t.Helper()
	prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), id)
	require.NoError(t, err)
	defer prepared.Abort()
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	return committed.SessionHandle
}

func TestStoppedSessionViewManualReactivation(t *testing.T) {
	for _, lostAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "acknowledged", true: "lost acknowledgement"}[lostAck], func(t *testing.T) {
			store := &reactivationAckStore{SQLiteSessionStore: coordinationSQLite(t).(*session.SQLiteSessionStore), loseAck: lostAck}
			stoppedViewFixture(t, store)
			var calls atomic.Int32
			rt, owner := stoppedViewRuntime(t, store, &calls)
			for _, id := range []string{"parent", "target", "descendant"} {
				handle := openStoppedView(t, owner, id)
				observation, err := handle.Observe(t.Context(), ObserveOptions{})
				require.NoError(t, err)
				assert.Len(t, observation.Initial[0].Session.OwnMessages(), 2)
				observation.Cancel()
				assert.True(t, handle.(*sessionHandle).driver.isStopped())
				assert.False(t, handle.(*sessionHandle).driver.WakePending())
			}
			target := openStoppedView(t, owner, "target")
			assert.Same(t, target.(*sessionHandle).driver, openStoppedView(t, owner, "target").(*sessionHandle).driver)
			assert.EqualValues(t, 0, calls.Load())
			_, err := target.Retry(t.Context())
			require.Error(t, err)
			_, err = target.Steer(t.Context(), TurnInput{Content: "steer"})
			require.Error(t, err)
			_, err = target.Submit(t.Context(), TurnInput{Retry: true, Content: "retry"})
			require.Error(t, err)
			_, err = target.Submit(t.Context(), TurnInput{})
			require.Error(t, err)
			_, err = target.Submit(t.Context(), TurnInput{Content: "obsolete pending target", RequestID: "old"})
			require.Error(t, err)
			_, err = target.Edit(t.Context(), SessionEdit{Kind: SessionEditResume})
			require.Error(t, err)
			_, err = rt.subagents.sendToChild("parent", "target", "agent send")
			require.Error(t, err)
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = target.Submit(canceled, TurnInput{Content: "canceled"})
			require.ErrorIs(t, err, context.Canceled)
			records, err := store.LoadChildren(t.Context(), "root")
			require.NoError(t, err)
			for _, record := range records {
				next := record
				next.Node.State = subagent.NodeIdle
				require.Error(t, store.CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: record.Revision, Record: next}))
			}
			observation, err := target.Observe(t.Context(), ObserveOptions{})
			require.NoError(t, err)
			defer observation.Cancel()
			submission, err := target.Submit(t.Context(), TurnInput{Content: "fresh manual message", RequestID: "fresh"})
			require.NoError(t, err)
			coordinationAwait(t, target, submission.TurnID)
			duplicate, err := target.Submit(t.Context(), TurnInput{Content: "fresh manual message", RequestID: "fresh"})
			require.NoError(t, err)
			assert.Equal(t, submission.TurnID, duplicate.TurnID)
			assert.EqualValues(t, 1, calls.Load())
			require.NoError(t, owner.Shutdown(t.Context()))
			rt2, owner2 := stoppedViewRuntime(t, store, &calls)
			restored := openStoppedView(t, owner2, "target")
			assert.False(t, restored.(*sessionHandle).driver.isStopped())
			assert.True(t, restored.(*sessionHandle).driver.Status().Dormant)
			records, err = store.LoadChildren(t.Context(), "root")
			require.NoError(t, err)
			require.Len(t, records, 3)
			for _, record := range records {
				expected := subagent.NodeStopped
				if record.Node.SessionID == "target" {
					expected = subagent.NodeIdle
					assert.Equal(t, submission.TurnID, record.LastTurnID)
				}
				assert.Equal(t, expected, record.Node.State)
				node, ok := rt2.subagents.tree.Node(record.Node.ID)
				require.True(t, ok)
				assert.Equal(t, expected, node.State)
			}
			saved, err := store.GetSession(t.Context(), "target")
			require.NoError(t, err)
			items := saved.MessagesSnapshot()
			assert.Equal(t, "retained history target", items[0].Message.Message.Content)
			assert.True(t, items[1].Message.Pending)
			assert.False(t, items[1].Message.Accepted)
			assert.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestStoppedSessionViewFences(t *testing.T) {
	for _, action := range []string{"abort", "stop", "delete", "revision"} {
		t.Run(action, func(t *testing.T) {
			store := coordinationSQLite(t)
			stoppedViewFixture(t, store)
			var calls atomic.Int32
			rt, owner := stoppedViewRuntime(t, store, &calls)
			target := openStoppedView(t, owner, "target")
			prepared, err := owner.Runtime().(SessionViewPreparer).PrepareSessionView(t.Context(), "target")
			require.NoError(t, err)
			switch action {
			case "abort":
				prepared.Abort()
			case "stop":
				_, err = rt.subagents.stopChild("parent", "target")
				require.NoError(t, err)
			case "delete":
				require.NoError(t, owner.Runtime().DeleteSession(t.Context(), "target"))
			case "revision":
				records, err := store.(session.CoordinationStore).LoadChildren(t.Context(), "root")
				require.NoError(t, err)
				for _, record := range records {
					if record.Node.SessionID == "target" {
						require.NoError(t, store.(session.CoordinationStore).CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: record.Revision, Record: record}))
					}
				}
			}
			if action != "revision" {
				_, err = prepared.Commit(t.Context())
				require.Error(t, err)
			} else {
				prepared.Abort()
			}
			if action != "abort" {
				_, err = target.Submit(t.Context(), TurnInput{Content: "must not run"})
				require.Error(t, err)
			}
			assert.EqualValues(t, 0, calls.Load())
			if action == "stop" {
				replacement := openStoppedView(t, owner, "target")
				assert.NotSame(t, target.(*sessionHandle).driver, replacement.(*sessionHandle).driver)
				_, err = target.Submit(t.Context(), TurnInput{Content: "stale"})
				require.Error(t, err)
				submission, err := replacement.Submit(t.Context(), TurnInput{Content: "fresh"})
				require.NoError(t, err)
				coordinationAwait(t, replacement, submission.TurnID)
				assert.EqualValues(t, 1, calls.Load())
			}
		})
	}
}

func TestStoppedSessionViewConcurrentFreshSubmission(t *testing.T) {
	store := coordinationSQLite(t)
	stoppedViewFixture(t, store)
	var calls atomic.Int32
	_, owner := stoppedViewRuntime(t, store, &calls)
	target := openStoppedView(t, owner, "target")
	results := make(chan Submission, 8)
	failures := make(chan error, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			submission, err := target.Submit(t.Context(), TurnInput{Content: "one fresh turn", RequestID: "same fresh id"})
			results <- submission
			failures <- err
		})
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	for submission := range results {
		coordinationAwait(t, target, submission.TurnID)
	}
	assert.EqualValues(t, 1, calls.Load())
}
