package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func TestSessionLegacyBatchSingleRunFollowupDormancyAndV2Queue(t *testing.T) {
	for _, sqlite := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "sqlite"}[sqlite], func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			if sqlite {
				store = coordinationSQLite(t)
			}
			var mu sync.Mutex
			var requests [][]chat.Message
			provider := coordinationReply("answer")
			provider.call = func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
				mu.Lock()
				requests = append(requests, messages)
				mu.Unlock()
				return newStreamBuilder().AddContent("answer").AddStopWithUsage(1, 1).Build(), nil
			}
			rt, owner := coordinationRuntime(t, store, provider, coordinationReply("child"))
			handle := coordinationCreate(t, owner.Runtime(), "root", "")
			legacy := handle.(SessionLegacyInput)
			inputs := []TurnInput{{Content: "first"}, {Content: "second"}}
			submission, err := legacy.RunLegacyTurn(t.Context(), inputs, "batch")
			require.NoError(t, err)
			coordinationAwait(t, handle, submission.TurnID)
			mu.Lock()
			require.Len(t, requests, 1)
			var users []string
			for _, msg := range requests[0] {
				if msg.Role == chat.MessageRoleUser {
					users = append(users, msg.Content)
				}
			}
			mu.Unlock()
			assert.Equal(t, []string{"first", "second"}, users, "batch is one turn and control marker is not model input")
			retry, err := legacy.RunLegacyTurn(t.Context(), inputs, "batch")
			require.NoError(t, err)
			assert.Equal(t, submission.TurnID, retry.TurnID)
			_, err = legacy.RunLegacyTurn(t.Context(), []TurnInput{{Content: "changed"}}, "batch")
			require.ErrorIs(t, err, session.ErrWriteConflict)
			follow := []TurnInput{{Content: "follow-one"}, {Content: "follow-two"}}
			result, err := legacy.QueueLegacyFollowUps(t.Context(), follow, "follow")
			require.NoError(t, err)
			assert.False(t, result.Streaming)
			rt.sessionDrivers.signalWork()
			time.Sleep(20 * time.Millisecond) //nolint:forbidigo // Observe a bounded no-wake interval after signaling the asynchronous scheduler.
			mu.Lock()
			assert.Len(t, requests, 1, "idle headless followups do not wake")
			mu.Unlock()
			result, err = legacy.QueueLegacyFollowUps(t.Context(), follow, "follow")
			require.NoError(t, err)
			assert.True(t, result.Duplicate)
			status, err := legacy.LegacyQueueStatus(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 2, status.FollowUpDepth)
			next, err := handle.Submit(t.Context(), TurnInput{Content: "v2", RequestID: "v2"})
			require.NoError(t, err)
			coordinationAwait(t, handle, next.TurnID)
			coordinationSettled(t, handle)
			mu.Lock()
			assert.Len(t, requests, 4, "same FIFO executes both followups and v2 exactly once")
			mu.Unlock()
			empty, err := legacy.RunLegacyTurn(t.Context(), nil, "empty")
			require.NoError(t, err)
			coordinationAwait(t, handle, empty.TurnID)
			mu.Lock()
			defer mu.Unlock()
			assert.Len(t, requests, 5)
		})
	}
}

func TestSessionLegacyFollowupReceiptSurvivesRestoreAndDeletion(t *testing.T) {
	store := coordinationSQLite(t)
	_, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	h := coordinationCreate(t, owner.Runtime(), "root", "")
	inputs := []TurnInput{{Content: "one"}, {Content: "two"}}
	_, err := h.(SessionLegacyInput).QueueLegacyFollowUps(t.Context(), inputs, "follow")
	require.NoError(t, err)
	require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context())))
	loaded, err := store.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	rt, owner2 := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("child"))
	restored, err := owner2.Runtime().CreateSession(t.Context(), loaded, SessionBinding{})
	require.NoError(t, err)
	result, err := restored.(SessionLegacyInput).QueueLegacyFollowUps(t.Context(), inputs, "follow")
	require.NoError(t, err)
	assert.True(t, result.Duplicate)
	rt.sessionDrivers.signalWork()
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // Observe a bounded no-wake interval after signaling the asynchronous scheduler.
	status, err := restored.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, status.Pending)
	run, err := restored.(SessionLegacyInput).RunLegacyTurn(t.Context(), nil, "run")
	require.NoError(t, err)
	coordinationAwait(t, restored, run.TurnID)
	coordinationSettled(t, restored)
	require.NoError(t, rt.DeleteSession(t.Context(), restored.ID()))
}

func TestSessionLegacyConcurrentV2AdmissionRejectsBusyWithoutPartialAppend(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	provider := coordinationReply("answer")
	provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return newStreamBuilder().AddContent("answer").AddStopWithUsage(1, 1).Build(), nil
	}
	store := coordinationSQLite(t)
	_, owner := coordinationRuntime(t, store, provider, coordinationReply("child"))
	h := coordinationCreate(t, owner.Runtime(), "root", "")
	submitted, err := h.Submit(t.Context(), TurnInput{Content: "active"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	_, err = h.(SessionLegacyInput).RunLegacyTurn(t.Context(), []TurnInput{{Content: "must not append"}}, "busy")
	require.Error(t, err)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorConflict, sessionErr.Kind)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	for _, message := range snapshot.OwnMessages() {
		assert.NotEqual(t, "must not append", message.Message.Content)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := h.(SessionLegacyInput).QueueLegacyFollowUps(t.Context(), []TurnInput{{Content: "once"}}, "same")
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	status, err := h.(SessionLegacyInput).LegacyQueueStatus(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, status.FollowUpDepth)
	close(release)
	coordinationAwait(t, h, submitted.TurnID)
	coordinationSettled(t, h)
	assert.EqualValues(t, 2, calls.Load())
}

type ambiguousBatchStore struct {
	session.Store
	session.ItemBatchAppender

	once atomic.Bool
}

func (s *ambiguousBatchStore) AppendItems(ctx context.Context, sessionID, requestID string, items []session.Item) (session.ItemBatchReceipt, error) {
	receipt, err := s.ItemBatchAppender.AppendItems(ctx, sessionID, requestID, items)
	if err == nil && s.once.CompareAndSwap(false, true) {
		return receipt, errors.New("ambiguous committed acknowledgement")
	}
	return receipt, err
}

func TestSessionLegacyBatchReconcilesAmbiguousAcknowledgement(t *testing.T) {
	base := session.NewInMemorySessionStore()
	store := &ambiguousBatchStore{Store: base, ItemBatchAppender: base.(session.ItemBatchAppender)}
	_, owner := coordinationRuntime(t, store, coordinationReply("answer"), coordinationReply("child"))
	h := coordinationCreate(t, owner.Runtime(), "root", "")
	legacy := h.(SessionLegacyInput)
	inputs := []TurnInput{{Content: "one"}, {Content: "two"}}
	_, err := legacy.RunLegacyTurn(t.Context(), inputs, "same")
	require.ErrorContains(t, err, "ambiguous")
	sub, err := legacy.RunLegacyTurn(t.Context(), inputs, "same")
	require.NoError(t, err)
	coordinationAwait(t, h, sub.TurnID)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	counts := map[string]int{}
	for _, msg := range snapshot.OwnMessages() {
		if msg.Message.Role == chat.MessageRoleUser && msg.InputMode != "legacy_run" {
			counts[msg.Message.Content]++
		}
	}
	assert.Equal(t, map[string]int{"one": 1, "two": 1}, counts)
}

func TestSessionLegacyCommandsUsePreBatchAgentAndPersistActiveBinding(t *testing.T) {
	store := coordinationSQLite(t)
	rootProvider, workerProvider, otherProvider := coordinationReply("root"), coordinationReply("worker"), coordinationReply("other")
	var calls atomic.Int32
	otherProvider.call = func(_ context.Context, messages []chat.Message) (chat.MessageStream, error) {
		calls.Add(1)
		var users []string
		for _, msg := range messages {
			if msg.Role == chat.MessageRoleUser {
				users = append(users, msg.Content)
			}
		}
		assert.Equal(t, []string{"plan the task", "next", "/plan non-user"}, users)
		return newStreamBuilder().AddContent("other").AddStopWithUsage(1, 1).Build(), nil
	}
	tm := team.New(team.WithAgents(
		agent.New("root", "root", agent.WithModel(rootProvider), agent.WithCommands(types.Commands{"plan": {Agent: "worker", Instruction: "plan"}, "next": {Agent: "other"}})),
		agent.New("worker", "worker", agent.WithModel(workerProvider), agent.WithCommands(types.Commands{"next": {Agent: "worker"}})),
		agent.New("other", "other", agent.WithModel(otherProvider)),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithSessionCompaction(false))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	h := coordinationCreate(t, owner.Runtime(), "root", "")
	messages := []LegacyCommandMessage{{Role: chat.MessageRoleUser, Input: TurnInput{Content: "/plan the task"}}, {Role: chat.MessageRoleUser, Input: TurnInput{Content: "/next next"}}, {Role: chat.MessageRoleAssistant, Input: TurnInput{Content: "/plan non-user"}}}
	legacy := h.(SessionLegacyCommandInput)
	sub, err := legacy.RunLegacyCommandTurn(t.Context(), messages, "", "command")
	require.NoError(t, err)
	coordinationAwait(t, h, sub.TurnID)
	assert.Equal(t, "other", h.AgentName())
	assert.EqualValues(t, 1, calls.Load())
	retry, err := legacy.RunLegacyCommandTurn(t.Context(), messages, "", "command")
	require.NoError(t, err)
	assert.Equal(t, sub.TurnID, retry.TurnID)
	assert.EqualValues(t, 1, calls.Load(), "retry does not resolve against new active agent")
	loaded, err := store.GetSession(t.Context(), h.ID())
	require.NoError(t, err)
	assert.Equal(t, "root", loaded.AttributesSnapshot()[SessionAgentAttribute])
	assert.Equal(t, "other", loaded.AgentName)
	require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context())))
	restarted, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store), WithSessionCompaction(false))
	require.NoError(t, err)
	restartedOwner := NewSessionRuntimeSupervisor(restarted)
	t.Cleanup(func() { require.NoError(t, restartedOwner.Shutdown(context.WithoutCancel(t.Context()))) })
	restored, err := restartedOwner.Runtime().CreateSession(t.Context(), loaded, SessionBinding{})
	require.NoError(t, err)
	assert.Equal(t, "other", restored.AgentName())
}

func TestSessionLegacyInvalidCommandTargetDoesNotMutateBatch(t *testing.T) {
	store := session.NewInMemorySessionStore()
	tm := team.New(team.WithAgents(agent.New("root", "root", agent.WithModel(coordinationReply("root")), agent.WithCommands(types.Commands{"bad": {Agent: "missing"}}))))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(store))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	h := coordinationCreate(t, owner.Runtime(), "root", "")
	_, err = h.(SessionLegacyCommandInput).RunLegacyCommandTurn(t.Context(), []LegacyCommandMessage{{Role: chat.MessageRoleUser, Input: TurnInput{Content: "/bad"}}}, "", "bad")
	require.Error(t, err)
	snapshot, err := h.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Empty(t, snapshot.MessagesSnapshot())
	assert.Equal(t, "root", h.AgentName())
}
