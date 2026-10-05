package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSubagentToolsRequireDirectCommittedParent(t *testing.T) {
	store := coordinationSQLite(t)
	worker := agent.New("worker", "prompt", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}), agent.WithToolSets(subagent.NewToolSet()))
	rootAgent := agent.New("root", "prompt", agent.WithModel(coordinationReply("unused")), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}), agent.WithToolSets(subagent.NewToolSet()))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(rootAgent, worker)), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	root := coordinationCreate(t, owner.Runtime(), "relations-root", "")
	a := coordinationCreate(t, owner.Runtime(), "relations-A", root.ID())
	b := coordinationCreate(t, owner.Runtime(), "relations-B", root.ID())
	c := coordinationCreate(t, owner.Runtime(), "relations-C", a.ID())
	other := coordinationCreate(t, owner.Runtime(), "relations-other", "")
	foreign := coordinationCreate(t, owner.Runtime(), "relations-foreign", other.ID())
	_, err = b.Edit(t.Context(), SessionEdit{Kind: SessionEditMessage, MessageIndex: -1, Message: session.UserMessage("private transcript")})
	require.NoError(t, err)
	advertised, err := worker.Tools(t.Context())
	require.NoError(t, err)
	call := func(caller, target SessionHandle, name string) *tools.ToolCallResult {
		t.Helper()
		node, ok := rt.subagents.nodeForSession(target.ID())
		require.True(t, ok)
		args, _ := json.Marshal(map[string]any{"subagent_id": string(node), "full": true})
		snap, err := caller.Snapshot(t.Context())
		require.NoError(t, err)
		snap.SetSafetyPolicy(session.SafetyPolicyAutonomous)
		var result *tools.ToolCallResult
		rt.processToolCalls(t.Context(), snap, []tools.ToolCall{{ID: name + target.ID(), Function: tools.FunctionCall{Name: name, Arguments: string(args)}}}, advertised, EventSinkFunc(func(event Event) {
			if output, ok := event.(*ToolCallResponseEvent); ok {
				result = output.Result
			}
		}))
		require.NotNil(t, result)
		return result
	}
	for _, pair := range []struct{ caller, target SessionHandle }{{a, b}, {root, c}, {a, foreign}, {a, a}, {c, a}} {
		require.True(t, call(pair.caller, pair.target, subagent.ToolReadSubagent).IsError)
		require.True(t, call(pair.caller, pair.target, subagent.ToolStopSubagent).IsError)
	}
	require.False(t, call(a, c, subagent.ToolReadSubagent).IsError)
	require.False(t, call(a, c, subagent.ToolStopSubagent).IsError)
	bNode, _ := rt.subagents.nodeForSession(b.ID())
	aSnap, err := a.Snapshot(t.Context())
	require.NoError(t, err)
	compatArgs, _ := json.Marshal(map[string]string{"task_id": string(bNode)})
	tc := tools.ToolCall{Function: tools.FunctionCall{Arguments: string(compatArgs)}}
	read, err := rt.handleBackgroundView(t.Context(), aSnap, tc, nil, nil)
	require.NoError(t, err)
	require.False(t, read.IsError)
	require.Contains(t, read.Output, "private transcript")
	stop, err := rt.handleBackgroundStop(t.Context(), aSnap, tc, nil, nil)
	require.NoError(t, err)
	require.False(t, stop.IsError)
	require.NoError(t, a.(SessionTreeController).StopSubtree(t.Context()), "authorized human may stop its target")
	require.NoError(t, b.(SessionTreeController).StopSubtree(t.Context()))
}

type childAdmissionBarrierStore struct {
	session.Store
	session.CoordinationStore

	parent     string
	entered    chan context.Context
	once       sync.Once
	armed      atomic.Bool
	admissions atomic.Int32
}

func (s *childAdmissionBarrierStore) GetSession(ctx context.Context, id string) (*session.Session, error) {
	if id == s.parent && s.armed.Load() {
		s.once.Do(func() { s.entered <- ctx })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.Store.GetSession(ctx, id)
}

func (s *childAdmissionBarrierStore) AdmitChild(ctx context.Context, a session.ChildAdmission) error {
	s.admissions.Add(1)
	return s.CoordinationStore.AdmitChild(ctx, a)
}

func TestChildAdmissionPreacceptHonorsCallerContext(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			base := coordinationSQLite(t)
			store := &childAdmissionBarrierStore{Store: base, CoordinationStore: base.(session.CoordinationStore), parent: t.Name() + "/root", entered: make(chan context.Context, 1)}
			rt, owner := coordinationRuntime(t, store, coordinationReply("unused"), coordinationReply("unused"))
			root := coordinationCreate(t, owner.Runtime(), store.parent, "")
			store.armed.Store(true)
			var ctx context.Context
			var cancel context.CancelFunc
			expected := context.Canceled
			if deadline {
				ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
				expected = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(t.Context())
			}
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := rt.CreateSession(ctx, session.New(session.WithID(t.Name()+"/child")), SessionBinding{AgentName: "worker", ParentSessionID: root.ID()})
				done <- err
			}()
			var ioctx context.Context
			select {
			case ioctx = <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("parent lookup not reached")
			}
			_, bounded := ioctx.Deadline()
			require.True(t, bounded)
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				require.ErrorIs(t, err, expected)
			case <-time.After(time.Second):
				t.Fatal("preaccept cancellation did not return")
			}
			require.ErrorIs(t, ioctx.Err(), expected)
			require.Zero(t, store.admissions.Load())
			records, err := base.(session.CoordinationStore).LoadChildren(t.Context(), root.ID())
			require.NoError(t, err)
			require.Empty(t, records)
		})
	}
}

type childLostAckStore struct {
	session.Store
	session.CoordinationStore
	session.ChildCommitBatchStore

	cancel           context.CancelFunc
	failReconcile    atomic.Bool
	admissionBounded atomic.Bool
	reconcileBounded atomic.Bool
}

func (s *childLostAckStore) AdmitChild(ctx context.Context, a session.ChildAdmission) error {
	_, bounded := ctx.Deadline()
	s.admissionBounded.Store(bounded)
	if err := s.CoordinationStore.AdmitChild(ctx, a); err != nil {
		return err
	}
	s.cancel()
	return errors.New("lost admission acknowledgement")
}

func (s *childLostAckStore) LoadChildren(ctx context.Context, root string) ([]session.ChildRecord, error) {
	_, bounded := ctx.Deadline()
	s.reconcileBounded.Store(bounded)
	if s.failReconcile.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.CoordinationStore.LoadChildren(ctx, root)
}

func TestChildAdmissionLostAckRetainsOwnership(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "reconciled", true: "root_stop_retry"}[fail], func(t *testing.T) {
			base := coordinationSQLite(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &childLostAckStore{Store: base, CoordinationStore: base.(session.CoordinationStore), ChildCommitBatchStore: base.(session.ChildCommitBatchStore), cancel: cancel}
			rt, owner := coordinationRuntime(t, store, coordinationReply("unused"), coordinationReply("unused"))
			root := coordinationCreate(t, owner.Runtime(), t.Name()+"/root", "")
			store.failReconcile.Store(fail)
			h, err := rt.CreateSession(ctx, session.New(session.WithID(t.Name()+"/child")), SessionBinding{AgentName: "worker", ParentSessionID: root.ID()})
			require.True(t, store.admissionBounded.Load())
			require.True(t, store.reconcileBounded.Load())
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			if fail {
				require.Error(t, err)
				require.Nil(t, h)
				rt.subagents.mu.Lock()
				pending := len(rt.subagents.admissions)
				rt.subagents.mu.Unlock()
				require.Equal(t, 1, pending)
				stopCtx, stopCancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				require.Error(t, root.(SessionTreeController).StopSubtree(stopCtx))
				stopCancel()
				store.failReconcile.Store(false)
				require.NoError(t, root.(SessionTreeController).StopSubtree(t.Context()))
				records, err := base.(session.CoordinationStore).LoadChildren(t.Context(), root.ID())
				require.NoError(t, err)
				require.Len(t, records, 1)
				require.Equal(t, subagent.NodeStopped, records[0].Node.State)
				rt.subagents.mu.Lock()
				pending = len(rt.subagents.admissions)
				rt.subagents.mu.Unlock()
				require.Zero(t, pending)
			} else {
				require.NoError(t, err)
				require.NotNil(t, h)
				submitted, err := h.Submit(t.Context(), TurnInput{Content: "execute after creator cancellation", RequestID: "detached-child"})
				require.NoError(t, err)
				coordinationAwait(t, h, submitted.TurnID)
				require.NoError(t, h.(SessionTreeController).StopSubtree(t.Context()))
			}
		})
	}
}
