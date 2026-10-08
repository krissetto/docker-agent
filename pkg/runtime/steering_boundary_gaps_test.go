package runtime

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestSteeringBoundaryUnresolvedApprovalRetainsAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		confirmation := make(chan *ToolCallConfirmationEvent, 1)
		observer := &fnObserver{onEvent: func(_ context.Context, _ *session.Session, event Event) {
			if prompt, ok := event.(*ToolCallConfirmationEvent); ok {
				confirmation <- prompt
			}
		}}
		var calls, executions atomic.Int32
		restartRelease := make(chan struct{})
		var release sync.Once
		var restarted []chat.Message
		provider := coordinationReply("done")
		provider.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
			switch calls.Add(1) {
			case 1:
				return newStreamBuilder().AddToolCallName("approved-call", "approval_tool").AddToolCallArguments("approved-call", "{}").AddToolCallStopWithUsage(1, 1).Build(), nil
			case 2:
				restarted = messages
				select {
				case <-restartRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
		}
		tool := tools.Tool{Name: "approval_tool", Parameters: map[string]any{"type": "object"}, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
			executions.Add(1)
			return tools.ResultSuccess("approved result"), nil
		}}
		store := session.NewInMemorySessionStore()
		r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider), agent.WithToolSets(newStubToolSet(nil, []tools.Tool{tool}, nil))))), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStore{}), WithEventObserver(observer))
		require.NoError(t, err)
		owner := NewSessionRuntimeSupervisor(r)
		t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
		t.Cleanup(func() { release.Do(func() { close(restartRelease) }) })
		h := coordinationCreate(t, owner.Runtime(), "approval-steering", "")
		active, err := h.Submit(t.Context(), TurnInput{Content: "active", RequestID: "active"})
		require.NoError(t, err)
		prompt := <-confirmation
		ordinary, err := h.Submit(t.Context(), TurnInput{Content: "ordinary queued", RequestID: "ordinary"})
		require.NoError(t, err)
		d := h.(*sessionHandle).driver
		input := agentCommunication("STEERING during approval", "approval-guidance", "sender", "worker", subagent.DeliveryGuidance)
		first, err := d.postCommunication(t.Context(), input)
		require.NoError(t, err)
		require.True(t, first.Accepted)
		require.True(t, first.Durable)
		retry, err := d.postCommunication(t.Context(), input)
		require.NoError(t, err)
		require.True(t, retry.Idempotent)
		synctest.Wait()
		require.Equal(t, int32(1), calls.Load(), "STEERING must not restart before approval resolves")
		require.Zero(t, executions.Load(), "STEERING cannot authorize a tool")
		require.Equal(t, active.TurnID, d.ActiveRequestID())
		observation, err := h.Observe(t.Context(), ObserveOptions{})
		require.NoError(t, err)
		defer observation.Cancel()
		require.Len(t, observation.Primary().Interactions, 1)
		require.Equal(t, prompt.RequestID, observation.Primary().Interactions[0].InteractionID)
		require.Error(t, h.Respond(t.Context(), InteractionResponse{InteractionID: "wrong-approval", Kind: InteractionConfirmation, Resume: ResumeApprove()}))
		require.NoError(t, h.Respond(t.Context(), InteractionResponse{InteractionID: prompt.RequestID, Kind: InteractionConfirmation, Resume: ResumeApprove()}))
		synctest.Wait()
		require.Equal(t, int32(1), executions.Load())
		require.Equal(t, int32(2), calls.Load())
		callAt, resultAt, guidanceAt := -1, -1, -1
		corrections := 0
		for i, message := range restarted {
			require.NotContains(t, message.Content, "ordinary queued")
			if len(message.ToolCalls) > 0 {
				callAt = i
			}
			if message.ToolCallID == "approved-call" {
				resultAt = i
				require.Equal(t, "approved result", message.Content)
			}
			if strings.Contains(message.Content, input.Content) {
				guidanceAt = i
				corrections++
			}
		}
		require.GreaterOrEqual(t, callAt, 0)
		require.Greater(t, resultAt, callAt)
		require.Greater(t, guidanceAt, resultAt)
		require.Equal(t, 1, corrections)
		coordinationAwait(t, h, first.RequestID)
		require.Error(t, h.Respond(t.Context(), InteractionResponse{InteractionID: prompt.RequestID, Kind: InteractionConfirmation, Resume: ResumeApprove()}), "resolved approval may not be reused")
		release.Do(func() { close(restartRelease) })
		coordinationAwait(t, h, ordinary.TurnID)
		coordinationSettled(t, h)
		require.Equal(t, int32(1), executions.Load())
	})
}

type steeringAdmissionGateStore struct {
	session.Store
	session.ItemAppender
	entered chan struct{}
	release <-chan struct{}
	writes  atomic.Int32
}

func (s *steeringAdmissionGateStore) AppendItem(ctx context.Context, id, key string, item session.Item) (int64, error) {
	if key == "input:blocked-steering" {
		if s.writes.Add(1) == 1 {
			close(s.entered)
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return s.ItemAppender.AppendItem(ctx, id, key, item)
}

func TestSteeringBoundaryConcurrentExactRetriesWaitForAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base := session.NewInMemorySessionStore()
		storeRelease := make(chan struct{})
		var release sync.Once
		store := &steeringAdmissionGateStore{Store: base, ItemAppender: base.(session.ItemAppender), entered: make(chan struct{}), release: storeRelease}
		r := newDriverTestRuntime(t)
		r.sessionStore = store
		sess := session.New(session.WithID("blocked-steering-admission"))
		require.NoError(t, store.AddSession(t.Context(), sess))
		d := r.sessionDrivers.Get(sess)
		require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionRunning; d.activeRequestID = "active"; return nil }))
		t.Cleanup(func() { release.Do(func() { close(storeRelease) }) })
		input := agentCommunication("STEERING correction", "blocked-steering", "sender", "worker", subagent.DeliveryGuidance)
		type result struct {
			receipt subagent.DeliveryReceipt
			err     error
		}
		firstDone, retries := make(chan result, 1), make(chan result, 8)
		go func() { receipt, err := d.postCommunication(t.Context(), input); firstDone <- result{receipt, err} }()
		<-store.entered
		for range 8 {
			go func() { receipt, err := d.postCommunication(t.Context(), input); retries <- result{receipt, err} }()
		}
		synctest.Wait()
		require.Equal(t, int32(1), store.writes.Load(), "exact retries wait behind first admission's durable lane")
		require.Empty(t, firstDone)
		require.Empty(t, retries)
		before, err := d.ownerSnapshot(t.Context())
		require.NoError(t, err)
		require.Empty(t, before.MessagesSnapshot(), "no volatile admission before durable acknowledgement")
		release.Do(func() { close(storeRelease) })
		first := <-firstDone
		require.NoError(t, first.err)
		require.True(t, first.receipt.Accepted)
		require.True(t, first.receipt.Durable)
		require.False(t, first.receipt.Queued)
		require.False(t, first.receipt.Idempotent)
		for range 8 {
			retry := <-retries
			require.NoError(t, retry.err)
			require.True(t, retry.receipt.Idempotent)
			retry.receipt.Idempotent = false
			require.Equal(t, first.receipt, retry.receipt)
		}
		require.Equal(t, int32(1), store.writes.Load())
		require.Len(t, d.drainBoundarySteering(), 1)
		require.Empty(t, d.drainBoundarySteering(), "exact retries cannot consume twice")
		persisted, err := store.GetSession(t.Context(), sess.ID)
		require.NoError(t, err)
		require.Len(t, persisted.MessagesSnapshot(), 1)
		require.False(t, persisted.MessagesSnapshot()[0].Message.Pending)
		accepted, promoted := 0, 0
		for _, event := range canonicalReplay(d) {
			switch event.Event.(type) {
			case *PendingUserMessageAcceptedEvent:
				accepted++
			case *PendingUserMessagePromotedEvent:
				promoted++
			}
		}
		require.Equal(t, 1, accepted)
		require.Equal(t, 1, promoted)
		require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionIdle; d.activeRequestID = ""; return nil }))
	})
}
