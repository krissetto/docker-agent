package runtime

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
)

type coordinationProvider struct {
	*mockProvider

	call func(context.Context, []chat.Message) (chat.MessageStream, error)
}

func (p *coordinationProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	return p.call(ctx, messages)
}

func coordinationReply(content string) *coordinationProvider {
	return &coordinationProvider{mockProvider: &mockProvider{id: "test/coordination"}, call: func(context.Context, []chat.Message) (chat.MessageStream, error) {
		return newStreamBuilder().AddContent(content).AddStopWithUsage(1, 1).Build(), nil
	}}
}

func coordinationWait(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("coordination barrier was not reached")
	}
}

func coordinationSQLite(t *testing.T) session.Store {
	t.Helper()
	store, err := sqlitestore.New(t.Context(), filepath.Join(t.TempDir(), "coordination.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func coordinationRuntime(t *testing.T, store session.Store, root, worker *coordinationProvider, opts ...Opt) (*LocalRuntime, SessionRuntimeSupervisor) {
	t.Helper()
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(root), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(worker)),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, append([]Opt{WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{})}, opts...)...)
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	return rt, owner
}

func coordinationCreate(t *testing.T, view SessionRuntime, id, parent string) SessionHandle {
	t.Helper()
	name := "root"
	if parent != "" {
		name = "worker"
	}
	h, err := view.CreateSession(t.Context(), session.New(session.WithID(id), session.WithTitle("Coordination test")), SessionBinding{AgentName: name, ParentSessionID: parent})
	require.NoError(t, err)
	return h
}

func coordinationAwait(t *testing.T, h SessionHandle, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, h.AwaitTurn(ctx, id))
}

func coordinationSettled(t *testing.T, h SessionHandle) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, err := h.Status(t.Context())
		return err == nil && status.State == SessionStateSettled && status.Pending == 0
	}, 5*time.Second, time.Millisecond)
}

func TestCoordinationShutdownCancelsCorrelatedSuccessor(t *testing.T) {
	firstEntered, firstRelease, secondEntered, secondCanceled := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		if calls.Add(1) == 1 {
			close(firstEntered)
			select {
			case <-firstRelease:
				return newStreamBuilder().AddContent("first done").AddStopWithUsage(1, 1).Build(), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		close(secondEntered)
		<-ctx.Done()
		close(secondCanceled)
		return nil, ctx.Err()
	}
	_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("report received"), worker)
	view := owner.Runtime()
	coordinationCreate(t, view, "root", "")
	child := coordinationCreate(t, view, "child", "root")
	first, err := child.Submit(t.Context(), TurnInput{Content: "first", RequestID: "first"})
	require.NoError(t, err)
	coordinationWait(t, firstEntered)
	second, err := child.Submit(t.Context(), TurnInput{Content: "second", RequestID: "second"})
	require.NoError(t, err)
	close(firstRelease)
	coordinationWait(t, secondEntered)
	coordinationAwait(t, child, first.TurnID)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, owner.Shutdown(ctx))
	coordinationWait(t, secondCanceled)
	coordinationAwait(t, child, second.TurnID)
}

func TestCoordinationReportsSurviveMailboxSaturation(t *testing.T) {
	store := session.NewInMemorySessionStore()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var calls atomic.Int32
	rootProvider := coordinationReply("received")
	rootProvider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		if calls.Add(1) == 1 {
			return newStreamBuilder().AddToolCallName("hold", "hold_reports").AddToolCallArguments("hold", `{}`).AddToolCallStopWithUsage(1, 1).Build(), nil
		}
		return newStreamBuilder().AddContent("received").AddStopWithUsage(1, 1).Build(), nil
	}
	hold := tools.Tool{Name: "hold_reports", Parameters: map[string]any{}, Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		close(entered)
		select {
		case <-release:
			return tools.ResultSuccess("released"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(rootProvider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}), agent.WithToolSets(newStubToolSet(nil, []tools.Tool{hold}, nil))),
		agent.New("worker", "prompt", agent.WithModel(coordinationReply("child result"))),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	view := owner.Runtime()
	root, err := view.CreateSession(t.Context(), session.New(session.WithID("root"), session.WithTitle("Coordination test"), session.WithToolsApproved(true)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	_, err = root.Submit(t.Context(), TurnInput{Content: "hold root"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	for i := range 100 {
		child := coordinationCreate(t, view, fmt.Sprintf("child-%03d", i), root.ID())
		submission, submitErr := child.Submit(t.Context(), TurnInput{Content: "finish", RequestID: "finish"})
		require.NoError(t, submitErr)
		coordinationAwait(t, child, submission.TurnID)
	}
	coord := store.(session.CoordinationStore)
	require.Eventually(t, func() bool {
		status, statusErr := root.Status(t.Context())
		reports, reportErr := coord.PendingReports(t.Context(), root.ID())
		return statusErr == nil && reportErr == nil && status.Pending == 64 && len(reports) == 36
	}, 5*time.Second, time.Millisecond)
	releaseOnce.Do(func() { close(release) })
	require.Eventually(t, func() bool {
		reports, reportErr := coord.PendingReports(t.Context(), root.ID())
		return reportErr == nil && len(reports) == 0
	}, 5*time.Second, time.Millisecond)
	coordinationSettled(t, root)
	snapshot, err := root.Snapshot(t.Context())
	require.NoError(t, err)
	reports := map[string]int{}
	for _, item := range snapshot.MessagesSnapshot() {
		if item.Message != nil && strings.HasPrefix(item.Message.TurnID, "report:") {
			reports[item.Message.TurnID]++
			assert.False(t, item.Message.Pending)
		}
	}
	require.Len(t, reports, 100)
	for id, count := range reports {
		assert.Equal(t, 1, count, "report %s must be delivered exactly once", id)
	}
}

func TestCoordinationActiveChildPinsIdleParent(t *testing.T) {
	entered := make(chan struct{})
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	policy := DefaultSessionResourcePolicy()
	policy.MaxSessions, policy.IdleRetention = 2, 0
	_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("root"), worker, WithSessionResourcePolicy(policy))
	view := owner.Runtime()
	root := coordinationCreate(t, view, "root", "")
	child := coordinationCreate(t, view, "child", root.ID())
	_, err := child.Submit(t.Context(), TurnInput{Content: "hold child"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	_, err = view.CreateSession(t.Context(), session.New(session.WithID("pressure")), SessionBinding{AgentName: "root"})
	var capacity *SessionError
	require.ErrorAs(t, err, &capacity)
	assert.Equal(t, SessionErrorCapacity, capacity.Kind)
	_, err = view.SessionByID(root.ID())
	require.NoError(t, err, "an idle parent remains reachable while its child is active")
}

func TestCoordinationHandoffChainAndTransferHooks(t *testing.T) {
	recorder := &recordingBuiltin{}
	hookConfig := &hooks.Config{OnAgentSwitch: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "record_coordination_switch"}}}
	toolProvider := func(name, args string) *coordinationProvider {
		p := coordinationReply("unused")
		p.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
			return newStreamBuilder().AddToolCallName(name, name).AddToolCallArguments(name, args).AddToolCallStopWithUsage(1, 1).Build(), nil
		}
		return p
	}
	worker := agent.New("worker", "prompt", agent.WithModel(coordinationReply("transfer result")))
	cProvider := coordinationReply("final result")
	var cCalls atomic.Int32
	cProvider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		if cCalls.Add(1) == 1 {
			return newStreamBuilder().AddToolCallName("transfer", "transfer_task").AddToolCallArguments("transfer", `{"agent":"worker","task":"do work"}`).AddToolCallStopWithUsage(1, 1).Build(), nil
		}
		return newStreamBuilder().AddContent("final result").AddStopWithUsage(1, 1).Build(), nil
	}
	c := agent.New("C", "prompt", agent.WithModel(cProvider), agent.WithSubAgents(worker), agent.WithToolSets(transfertask.New()), agent.WithHooks(hookConfig))
	b := agent.New("B", "prompt", agent.WithModel(toolProvider("handoff", `{"agent":"C"}`)), agent.WithHandoffs(c), agent.WithToolSets(handoff.New()), agent.WithHooks(hookConfig))
	root := agent.New("root", "prompt", agent.WithModel(toolProvider("handoff", `{"agent":"B"}`)), agent.WithHandoffs(b), agent.WithToolSets(handoff.New()), agent.WithHooks(hookConfig))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, b, c, worker)), WithSessionStore(session.NewInMemorySessionStore()), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	require.NoError(t, rt.hooksRegistry.RegisterBuiltin("record_coordination_switch", recorder.hook))
	rt.buildHooksExecutors()
	h := coordinationCreate(t, owner.Runtime(), "chain", "")
	policy := session.SafetyPolicyAutonomous
	_, err = h.Edit(t.Context(), SessionEdit{Kind: SessionEditPolicy, SafetyPolicy: &policy})
	require.NoError(t, err)
	submission, err := h.Submit(t.Context(), TurnInput{Content: "delegate"})
	require.NoError(t, err)
	coordinationAwait(t, h, submission.TurnID)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "final result", snapshot.GetLastAssistantMessageContent())
	var transitions []string
	for _, in := range recorder.snapshot() {
		transitions = append(transitions, in.FromAgent+">"+in.ToAgent+":"+in.AgentSwitchKind)
	}
	assert.Equal(t, []string{"root>B:handoff", "B>C:handoff", "C>worker:transfer_task", "worker>C:transfer_task_return"}, transitions)
}

type coordinationCommitStore struct {
	session.Store
	session.CoordinationStore

	sessionTree subagent.Store
	fail        atomic.Bool
	err         error
}

func (s *coordinationCommitStore) CommitChild(ctx context.Context, c session.ChildCommit) error {
	if s.fail.Load() {
		return s.err
	}
	return s.CoordinationStore.CommitChild(ctx, c)
}

func (s *coordinationCommitStore) SaveTree(ctx context.Context, id string, snapshot subagent.Snapshot) error {
	return s.sessionTree.SaveTree(ctx, id, snapshot)
}

func (s *coordinationCommitStore) LoadTree(ctx context.Context, id string) (*subagent.Snapshot, error) {
	return s.sessionTree.LoadTree(ctx, id)
}

func (*coordinationCommitStore) Durability() subagent.Durability { return subagent.DurabilityDurable }

func TestCoordinationCompletionFailureRemainsRetryable(t *testing.T) {
	base := coordinationSQLite(t)
	store := &coordinationCommitStore{Store: base, CoordinationStore: base.(session.CoordinationStore), sessionTree: base.(subagent.Store), err: errors.New("completion commit rejected")}
	store.fail.Store(true)
	_, owner := coordinationRuntime(t, store, coordinationReply("received"), coordinationReply("durable child result"))
	coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", "root")
	submission, err := child.Submit(t.Context(), TurnInput{Content: "work", RequestID: "work"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.ErrorIs(t, child.AwaitTurn(ctx, submission.TurnID), store.err)
	reports, err := store.PendingReports(t.Context(), "root")
	require.NoError(t, err)
	assert.Empty(t, reports, "failed commit cannot publish completion")
	row, err := store.GetSession(t.Context(), child.ID())
	require.NoError(t, err)
	assert.Equal(t, "durable child result", row.GetLastAssistantMessageContent())
	store.fail.Store(false)
	require.Eventually(t, func() bool {
		records, err := store.LoadChildren(t.Context(), "root")
		return err == nil && len(records) == 1 && records[0].LastTurnID == submission.TurnID
	}, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		status, statusErr := child.Status(t.Context())
		return statusErr == nil && status.State == SessionStateSettled && status.LastError == ""
	}, 5*time.Second, time.Millisecond)
	coordinationAwait(t, child, submission.TurnID)
}

func TestCoordinationQueuedChildRestoresOwnRowAndRequestIdentity(t *testing.T) {
	store := coordinationSQLite(t)
	entered := make(chan struct{})
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	_, owner := coordinationRuntime(t, store, coordinationReply("received"), worker)
	coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", "root")
	_, err := child.Submit(t.Context(), TurnInput{Content: "first", RequestID: "first"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	queued, err := child.Submit(t.Context(), TurnInput{Content: "queued durable", RequestID: "queued"})
	require.NoError(t, err)
	row, err := store.GetSession(t.Context(), child.ID())
	require.NoError(t, err)
	matches := 0
	for _, item := range row.MessagesSnapshot() {
		if item.Message != nil && item.Message.TurnID == queued.TurnID {
			matches++
			assert.True(t, item.Message.Pending)
			assert.True(t, item.Message.Accepted)
		}
	}
	require.Equal(t, 1, matches, "accepted queued input must already exist in the child's own row")
	require.NoError(t, owner.Shutdown(t.Context()))
	_, restored := coordinationRuntime(t, store, coordinationReply("received"), coordinationReply("restored result"))
	_, root, err := restored.Runtime().(SessionLoader).LoadSession(t.Context(), "root")
	require.NoError(t, err)
	require.NoError(t, restored.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), root))
	resumed, err := restored.Runtime().SessionByID(child.ID())
	require.NoError(t, err)
	coordinationAwait(t, resumed, queued.TurnID)
	again, err := resumed.Submit(t.Context(), TurnInput{Content: "queued durable", RequestID: "queued"})
	require.NoError(t, err)
	assert.Equal(t, queued.TurnID, again.TurnID)
	coordinationAwait(t, resumed, again.TurnID)
	snapshot, err := resumed.Snapshot(t.Context())
	require.NoError(t, err)
	matches = 0
	for _, item := range snapshot.MessagesSnapshot() {
		if item.Message != nil && item.Message.TurnID == queued.TurnID {
			matches++
			assert.False(t, item.Message.Pending)
		}
	}
	assert.Equal(t, 1, matches, "retrying the restored request must not duplicate transcript input")
}

func TestCoordinationReleasedChildRetainsMetadataNotPayload(t *testing.T) {
	store := coordinationSQLite(t)
	payload := strings.Repeat("large child transcript ", 4096)
	rt, owner := coordinationRuntime(t, store, coordinationReply("received"), coordinationReply(payload))
	root := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", root.ID())
	submission, err := child.Submit(t.Context(), TurnInput{Content: "write report"})
	require.NoError(t, err)
	coordinationAwait(t, child, submission.TurnID)
	require.Eventually(t, func() bool {
		reports, err := store.(session.CoordinationStore).PendingReports(t.Context(), root.ID())
		return err == nil && len(reports) == 0
	}, 5*time.Second, time.Millisecond)
	coordinationSettled(t, root)
	coordinationSettled(t, child)
	require.NoError(t, child.Release(t.Context()))
	_, err = owner.Runtime().SessionByID(child.ID())
	require.Error(t, err)
	rt.subagents.mu.Lock()
	for _, rec := range rt.subagents.children {
		if rec.sessionID == child.ID() {
			assert.Nil(t, rec.session, "manager must not retain the released child transcript")
			assert.Nil(t, rec.parentSess)
			assert.LessOrEqual(t, len([]rune(rec.result)), subagent.PreviewLen)
		}
	}
	if tracked := rt.subagents.sessions[child.ID()]; tracked != nil {
		assert.Nil(t, tracked.sess)
		assert.Nil(t, tracked.unwatch, "callbacks must not retain an evicted driver")
	}
	rt.subagents.mu.Unlock()
	row, err := store.GetSession(t.Context(), child.ID())
	require.NoError(t, err)
	storedPayload := ""
	for _, item := range row.MessagesSnapshot() {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
			storedPayload = item.Message.Message.Content
		}
	}
	assert.Len(t, storedPayload, len(payload), "releasing runtime payload must preserve durable browsing")
	assert.Equal(t, sha256.Sum256([]byte(payload)), sha256.Sum256([]byte(storedPayload)), "durable transcript bytes must remain unchanged")
	tree, err := owner.Runtime().(TreeInspector).InspectSessionTree(t.Context(), root.ID())
	require.NoError(t, err)
	require.Len(t, tree.Nodes, 1)
	require.Len(t, tree.Nodes[0].Children, 1)
	assert.Equal(t, child.ID(), tree.Nodes[0].Children[0].Node.SessionID)
}

type coordinationStartupStore struct {
	session.Store

	entered chan struct{}
	release chan struct{}
	armed   atomic.Bool
	started atomic.Bool
	written atomic.Bool
}

func (s *coordinationStartupStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	if !s.armed.Load() || !s.started.CompareAndSwap(false, true) {
		return s.Store.UpdateSession(ctx, sess)
	}
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := s.Store.UpdateSession(ctx, sess); err != nil {
		return err
	}
	s.written.Store(true)
	return nil
}

func TestCoordinationStartupMetadataPrecedesHandoffExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &coordinationStartupStore{
			Store:   session.NewInMemorySessionStore(),
			entered: make(chan struct{}),
			release: make(chan struct{}),
		}
		defer func() {
			select {
			case <-store.release:
			default:
				close(store.release)
			}
		}()
		var executionBeforeStartup atomic.Bool
		provider := coordinationReply("unused")
		provider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
			executionBeforeStartup.Store(!store.written.Load())
			return newStreamBuilder().AddToolCallName("handoff", "handoff").AddToolCallArguments("handoff", `{"agent":"finisher"}`).AddToolCallStopWithUsage(1, 1).Build(), nil
		}
		finisher := agent.New("finisher", "prompt", agent.WithModel(coordinationReply("finished")))
		rootAgent := agent.New("root", "prompt", agent.WithModel(provider), agent.WithHandoffs(finisher), agent.WithToolSets(handoff.New()))
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(rootAgent, finisher)), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		owner := NewSessionRuntimeSupervisor(rt)
		t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
		root := coordinationCreate(t, owner.Runtime(), "startup-handoff", "")
		policy := session.SafetyPolicyAutonomous
		_, err = root.Edit(t.Context(), SessionEdit{Kind: SessionEditPolicy, SafetyPolicy: &policy})
		require.NoError(t, err)
		store.armed.Store(true)
		submission, err := root.Submit(t.Context(), TurnInput{Content: "handoff after startup"})
		require.NoError(t, err)
		coordinationWait(t, store.entered)
		synctest.Wait()
		assert.False(t, executionBeforeStartup.Load(), "execution cannot overtake the blocked startup metadata write")
		close(store.release)
		coordinationAwait(t, root, submission.TurnID)
		assert.False(t, executionBeforeStartup.Load(), "provider must see completed startup persistence")
		require.Equal(t, "finisher", root.AgentName())
		persisted, err := store.GetSession(t.Context(), root.ID())
		require.NoError(t, err)
		assert.Equal(t, "finisher", persisted.AgentName, "startup metadata cannot overwrite the handoff")
	})
}

type coordinationStartupFailureStore struct {
	session.Store

	fail atomic.Bool
}

func (s *coordinationStartupFailureStore) UpdateSession(ctx context.Context, sess *session.Session) error {
	if s.fail.Load() {
		return assert.AnError
	}
	return s.Store.UpdateSession(ctx, sess)
}

func TestCoordinationStartupPersistenceFailureRetainsAcceptedInput(t *testing.T) {
	store := &coordinationStartupFailureStore{Store: coordinationSQLite(t)}
	var calls atomic.Int32
	provider := coordinationReply("unused")
	provider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		calls.Add(1)
		return newStreamBuilder().AddContent("recovered").AddStopWithUsage(1, 1).Build(), nil
	}
	_, owner := coordinationRuntime(t, store, provider, coordinationReply("child"))
	h := coordinationCreate(t, owner.Runtime(), "startup-failure", "")
	store.fail.Store(true)
	first, err := h.Submit(t.Context(), TurnInput{Content: "accepted before startup", RequestID: "first"})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.ErrorIs(t, h.AwaitTurn(ctx, first.TurnID), assert.AnError)
	assert.Zero(t, calls.Load(), "failed startup persistence must cancel before any provider call")
	_, err = h.Submit(t.Context(), TurnInput{Content: "blocked", RequestID: "second"})
	var blocked *SessionError
	require.ErrorAs(t, err, &blocked)
	assert.Equal(t, SessionErrorPersistence, blocked.Kind)
	persisted, err := store.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	messages := persisted.OwnMessages()
	require.Len(t, messages, 1)
	assert.Equal(t, first.TurnID, messages[0].TurnID)
	assert.True(t, messages[0].Accepted, "startup cancellation must retain the durable accepted input")
	store.fail.Store(false)
	require.Eventually(t, func() bool { return h.AwaitTurn(t.Context(), first.TurnID) == nil }, 2*time.Second, time.Millisecond)
	second, err := h.Submit(t.Context(), TurnInput{Content: "blocked", RequestID: "second"})
	require.NoError(t, err)
	coordinationAwait(t, h, second.TurnID)
	assert.Equal(t, int32(1), calls.Load(), "only the post-recovery turn executes")
	assert.Equal(t, "recovered", sessionHandleSnapshot(t, h).GetLastAssistantMessageContent())
}

func TestCoordinationHistoricalChildGrantSurvivesParentHandoffAndRestore(t *testing.T) {
	store := coordinationSQLite(t)
	var handoffToC atomic.Bool
	makeOwner := func() SessionRuntimeSupervisor {
		handoffProvider := func(target string) *coordinationProvider {
			p := coordinationReply("unused")
			p.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
				return newStreamBuilder().AddToolCallName("handoff", "handoff").AddToolCallArguments("handoff", fmt.Sprintf(`{"agent":%q}`, target)).AddToolCallStopWithUsage(1, 1).Build(), nil
			}
			return p
		}
		finisher := agent.New("finisher", "prompt", agent.WithModel(coordinationReply("child finished")))
		worker := agent.New("worker", "prompt", agent.WithModel(handoffProvider("finisher")), agent.WithHandoffs(finisher), agent.WithToolSets(handoff.New()))
		c := agent.New("C", "prompt", agent.WithModel(coordinationReply("C active")))
		bProvider := coordinationReply("B active")
		bProvider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
			if handoffToC.Load() {
				return newStreamBuilder().AddToolCallName("to-c", "handoff").AddToolCallArguments("to-c", `{"agent":"C"}`).AddToolCallStopWithUsage(1, 1).Build(), nil
			}
			return newStreamBuilder().AddContent("B active").AddStopWithUsage(1, 1).Build(), nil
		}
		b := agent.New("B", "prompt", agent.WithModel(bProvider), agent.WithHandoffs(c), agent.WithToolSets(handoff.New()), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"}))
		root := agent.New("root", "prompt", agent.WithModel(handoffProvider("B")), agent.WithHandoffs(b), agent.WithToolSets(handoff.New()))
		rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, b, c, worker, finisher)), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		owner := NewSessionRuntimeSupervisor(rt)
		t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
		return owner
	}
	owner := makeOwner()
	root := coordinationCreate(t, owner.Runtime(), "grant-root", "")
	policy := session.SafetyPolicyAutonomous
	_, err := root.Edit(t.Context(), SessionEdit{Kind: SessionEditPolicy, SafetyPolicy: &policy})
	require.NoError(t, err)
	first, err := root.Submit(t.Context(), TurnInput{Content: "handoff to B"})
	require.NoError(t, err)
	coordinationAwait(t, root, first.TurnID)
	require.Equal(t, "B", root.AgentName())
	child := coordinationCreate(t, owner.Runtime(), "grant-child", root.ID())
	work, err := child.Submit(t.Context(), TurnInput{Content: "work and handoff"})
	require.NoError(t, err)
	coordinationAwait(t, child, work.TurnID)
	require.Equal(t, "finisher", child.AgentName())
	require.Eventually(t, func() bool {
		reports, err := store.(session.CoordinationStore).PendingReports(t.Context(), root.ID())
		return err == nil && len(reports) == 0
	}, 5*time.Second, time.Millisecond)
	coordinationSettled(t, root)
	handoffToC.Store(true)
	next, err := root.Submit(t.Context(), TurnInput{Content: "handoff to C"})
	require.NoError(t, err)
	coordinationAwait(t, root, next.TurnID)
	require.Equal(t, "C", root.AgentName())
	persisted, err := store.GetSession(t.Context(), child.ID())
	require.NoError(t, err)
	assert.Equal(t, "B", persisted.AttributesSnapshot()[SessionParentAgentAttribute])
	assert.Equal(t, "worker", persisted.AttributesSnapshot()[SessionAgentAttribute])
	assert.Equal(t, "finisher", persisted.AgentName)
	require.NoError(t, owner.Shutdown(t.Context()))
	restored := makeOwner()
	resumedRoot, rootSnapshot, err := restored.Runtime().(SessionLoader).LoadSession(t.Context(), root.ID())
	require.NoError(t, err)
	require.NoError(t, restored.Runtime().(TreeRestorer).RestoreSessionTree(t.Context(), rootSnapshot))
	resumedChild, err := restored.Runtime().SessionByID(child.ID())
	require.NoError(t, err, "historical B grant remains valid after parent becomes C")
	assert.Equal(t, "C", resumedRoot.AgentName())
	assert.Equal(t, "finisher", resumedChild.AgentName(), "child active agent is distinct from its worker binding")
	continued, err := resumedChild.Submit(t.Context(), TurnInput{Content: "continue existing child"})
	require.NoError(t, err)
	coordinationAwait(t, resumedChild, continued.TurnID)
	snapshot, err := resumedChild.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "child finished", snapshot.GetLastAssistantMessageContent())
	_, err = restored.Runtime().CreateSession(t.Context(), session.New(session.WithID("unauthorized-new-child")), SessionBinding{AgentName: "worker", ParentSessionID: root.ID()})
	require.Error(t, err, "historical grant must not authorize new C spawns")
	tree, err := restored.Runtime().(TreeInspector).InspectSessionTree(t.Context(), root.ID())
	require.NoError(t, err)
	require.Len(t, tree.Nodes, 1)
	require.Len(t, tree.Nodes[0].Children, 1)
	assert.Equal(t, "worker", tree.Nodes[0].Children[0].Node.Agent)
	assert.Equal(t, child.ID(), tree.Nodes[0].Children[0].Node.SessionID)
}

func TestCoordinationRetryRequestIDDeduplicatesQueuedAndCompleted(t *testing.T) {
	firstEntered, firstRelease, secondEntered, secondRelease := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := coordinationReply("unused")
	provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		var release <-chan struct{}
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			release = firstRelease
		case 2:
			close(secondEntered)
			release = secondRelease
		default:
			return newStreamBuilder().AddContent("unexpected duplicate").AddStopWithUsage(1, 1).Build(), nil
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
	}
	_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), provider, coordinationReply("worker"))
	h := coordinationCreate(t, owner.Runtime(), "retry-root", "")
	first, err := h.Submit(t.Context(), TurnInput{Content: "first", RequestID: "first"})
	require.NoError(t, err)
	coordinationWait(t, firstEntered)
	input := TurnInput{Retry: true, RequestID: "retry-once"}
	retry, err := h.Submit(t.Context(), input)
	require.NoError(t, err)
	repeated, err := h.Submit(t.Context(), input)
	require.NoError(t, err)
	assert.Equal(t, retry.TurnID, repeated.TurnID)
	status, err := h.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, status.Pending)
	var conflict *SessionError
	for range 2 {
		_, err = h.Submit(t.Context(), TurnInput{RequestID: input.RequestID})
		require.ErrorAs(t, err, &conflict)
		assert.Equal(t, SessionErrorConflict, conflict.Kind, "same ID cannot change retry into ordinary submit")
	}
	close(firstRelease)
	coordinationWait(t, secondEntered)
	coordinationAwait(t, h, first.TurnID)
	activeDuplicate, err := h.Submit(t.Context(), input)
	require.NoError(t, err)
	assert.Equal(t, retry.TurnID, activeDuplicate.TurnID)
	close(secondRelease)
	coordinationAwait(t, h, retry.TurnID)
	completedDuplicate, err := h.Submit(t.Context(), input)
	require.NoError(t, err)
	assert.Equal(t, retry.TurnID, completedDuplicate.TurnID)
	coordinationAwait(t, h, completedDuplicate.TurnID)
	coordinationSettled(t, h)
	assert.Equal(t, int32(2), calls.Load(), "one ordinary turn plus one retry; duplicates never execute")
	for range 2 {
		_, err = h.Submit(t.Context(), TurnInput{RequestID: input.RequestID})
		require.ErrorAs(t, err, &conflict)
		assert.Equal(t, SessionErrorConflict, conflict.Kind, "completed retry ID retains its mode")
	}
	for range 2 {
		_, err = h.Submit(t.Context(), TurnInput{Retry: true, RequestID: "first"})
		require.ErrorAs(t, err, &conflict)
		assert.Equal(t, SessionErrorConflict, conflict.Kind, "rejected mode changes must not become deduplicated retries")
	}
	assert.Equal(t, int32(2), calls.Load(), "rejected requests never execute")
}

func TestCoordinationRejectedRetryCapacityDoesNotConsumeRequestID(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := coordinationReply("unused")
	provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
	}
	policy := DefaultSessionResourcePolicy()
	policy.MailboxMessages = 1
	_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), provider, coordinationReply("worker"), WithSessionResourcePolicy(policy))
	h := coordinationCreate(t, owner.Runtime(), "retry-capacity", "")
	first, err := h.Submit(t.Context(), TurnInput{Content: "first", RequestID: "first"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	second, err := h.Submit(t.Context(), TurnInput{Content: "second", RequestID: "second"})
	require.NoError(t, err)
	retryInput := TurnInput{Retry: true, RequestID: "capacity-retry"}
	for range 2 {
		_, err = h.Submit(t.Context(), retryInput)
		var capacity *SessionError
		require.ErrorAs(t, err, &capacity)
		assert.Equal(t, SessionErrorCapacity, capacity.Kind, "rejected retry must remain rejected while mailbox is full")
	}
	assert.Equal(t, int32(1), calls.Load())
	close(release)
	coordinationAwait(t, h, first.TurnID)
	coordinationAwait(t, h, second.TurnID)
	retry, err := h.Submit(t.Context(), retryInput)
	require.NoError(t, err, "the same rejected request ID can be admitted after capacity is free")
	coordinationAwait(t, h, retry.TurnID)
	duplicate, err := h.Submit(t.Context(), retryInput)
	require.NoError(t, err)
	assert.Equal(t, retry.TurnID, duplicate.TurnID)
	coordinationAwait(t, h, duplicate.TurnID)
	assert.Equal(t, int32(3), calls.Load(), "two ordinary turns and exactly one accepted retry execute")
}

func TestCoordinationCreateSessionDetachesCallerSnapshot(t *testing.T) {
	_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("root"), coordinationReply("worker"))
	original := session.New(session.WithID("detached"), session.WithTitle("original title"), session.WithSafetyPolicy(session.SafetyPolicyStrict), session.WithUserMessage("original message"))
	h, err := owner.Runtime().CreateSession(t.Context(), original, SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	original.SetTitle("caller mutation")
	original.SetSafetyPolicy(session.SafetyPolicyAutonomous)
	original.Messages[0].Message.Message.Content = "mutated original message"
	original.AddMessage(session.UserMessage("caller addition"))
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "original title", snapshot.TitleSnapshot())
	assert.Equal(t, session.SafetyPolicyStrict, snapshot.GetSafetyPolicy())
	require.Len(t, snapshot.Messages, 1)
	assert.Equal(t, "original message", snapshot.Messages[0].Message.Message.Content)
	_, err = h.Edit(t.Context(), SessionEdit{Kind: SessionEditTitle, Title: "runtime edit"})
	require.NoError(t, err)
	assert.Equal(t, "caller mutation", original.TitleSnapshot(), "handle edits must not write through to caller-owned input")
	repeated, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID(original.ID)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	assert.Equal(t, h.ID(), repeated.ID())
	current, err := repeated.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "runtime edit", current.TitleSnapshot(), "repeated binding must not replace live state with the new template")
}
