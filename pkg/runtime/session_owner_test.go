package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestOwnerApprovalHasOneWinnerWhileReceiverDrains(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionRunning; d.generation = 1; d.activeRequestID = "turn"; return nil }))
	resume, err := d.registerResume(t.Context(), "approval")
	require.NoError(t, err)
	var winners atomic.Int32
	var workers sync.WaitGroup
	drainCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for {
			select {
			case <-resume:
			case <-drainCtx.Done():
				return
			}
		}
	}()
	for range 64 {
		workers.Go(func() {
			if d.Respond(InteractionResponse{InteractionID: "approval", Kind: InteractionConfirmation, Resume: ResumeApprove()}) == nil {
				winners.Add(1)
			}
		})
	}
	workers.Wait()
	assert.Equal(t, int32(1), winners.Load())
}

func TestOwnerElicitationRejectsDifferentWaiter(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	first, second := newElicitationWaiter(), newElicitationWaiter()
	event := ElicitationRequest("first", "form", nil, "", "first-waiter", "", d.identityID, nil, "root").(*ElicitationRequestEvent)
	require.NoError(t, d.registerElicitation(t.Context(), "first", event, first))
	event2 := ElicitationRequest("second", "form", nil, "", "second-waiter", "", d.identityID, nil, "root").(*ElicitationRequestEvent)
	require.NoError(t, d.registerElicitation(t.Context(), "second", event2, second))
	err := d.Respond(InteractionResponse{InteractionID: "first", Kind: InteractionElicitation, ElicitationID: "second-waiter", Elicitation: ElicitationResult{Action: tools.ElicitationActionAccept}})
	require.Error(t, err)
	assert.Equal(t, int32(waiterPending), first.state.Load())
	assert.Equal(t, int32(waiterPending), second.state.Load())
	require.NoError(t, d.Respond(InteractionResponse{InteractionID: "first", Kind: InteractionElicitation, ElicitationID: "first-waiter", Elicitation: ElicitationResult{Action: tools.ElicitationActionAccept}}))
}

func TestOwnerExecutionAppendPreservesConcurrentAcceptedInput(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionRunning; d.generation = 1; d.activeRequestID = "turn"; return nil }))
	_, scratch, err := d.executionContext(t.Context(), 1)
	require.NoError(t, err)
	require.NoError(t, r.Steer(t.Context(), QueuedMessage{Content: "concurrent", RequestID: "concurrent"}))
	message := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "answer"})
	scratch.AddMessage(message)
	require.NoError(t, d.commitExecutionEvent(t.Context(), 1, MessageAddedAt(d.identityID, message, "root", 0)))
	snapshot, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Messages, 2)
	assert.Equal(t, "concurrent", snapshot.Messages[0].Message.TurnID)
	assert.True(t, snapshot.Messages[0].Message.Pending)
	assert.Equal(t, "answer", snapshot.Messages[1].Message.Message.Content)
	assert.NotSame(t, message, snapshot.Messages[1].Message)
	require.Error(t, d.commitExecutionEvent(t.Context(), 2, MessageAddedAt(d.identityID, message, "root", 0)))
	snapshot, err = d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	assert.Len(t, snapshot.Messages, 2)
}

func TestOwnerCallJoinsStartedStateTransitionAfterCancellation(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	ctx, cancel := context.WithCancel(t.Context())
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	result := 0
	go func() { done <- d.ownerCall(ctx, func() error { close(entered); <-release; result = 1; return nil }) }()
	<-entered
	cancel()
	select {
	case <-done:
		t.Fatal("started owner transition returned before transferring its outputs")
	default:
	}
	close(release)
	require.NoError(t, <-done)
	assert.Equal(t, 1, result)
}

func TestRuntimeReportDoesNotInterruptProviderAttempt(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.phase = sessionRunning; d.generation = 1; d.activeRequestID = "turn"; return nil }))
	attempt := d.steeringContext(t.Context())
	report := QueuedMessage{Content: "child finished", RequestID: "report", InputOrigin: session.InputOriginRuntime, InputMode: "steer"}
	require.True(t, d.Post(t.Context(), report, false))
	select {
	case <-steeringSignal(attempt):
		t.Fatal("coordination report interrupted provider output")
	default:
	}
	batch := d.drainBoundarySteering()
	require.Len(t, batch, 1)
	assert.Equal(t, "report", batch[0].RequestID)
	require.NoError(t, r.Steer(t.Context(), QueuedMessage{Content: "urgent user steer", RequestID: "user"}))
	select {
	case <-steeringSignal(attempt):
	default:
		t.Fatal("explicit user steering must still interrupt at the provider boundary")
	}
}

func TestSameSessionInteractionResponsesCannotConsumeOtherRequest(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	first, err := d.registerResume(t.Context(), "first")
	require.NoError(t, err)
	second, err := d.registerResume(t.Context(), "second")
	require.NoError(t, err)
	require.NoError(t, d.Respond(InteractionResponse{InteractionID: "second", Kind: InteractionConfirmation, Resume: ResumeReject("second only")}))
	select {
	case <-first:
		t.Fatal("response routed to unrelated request")
	default:
	}
	response := <-second
	assert.Equal(t, "second", response.RequestID)
	require.NoError(t, d.Respond(InteractionResponse{InteractionID: "first", Kind: InteractionConfirmation, Resume: ResumeApprove()}))
	assert.Equal(t, "first", (<-first).RequestID)
	require.Error(t, d.Respond(InteractionResponse{InteractionID: "first", Kind: InteractionConfirmation, Resume: ResumeApprove()}))
}

func TestAcceptedTurnOutlivesSubmissionContext(t *testing.T) {
	rt, seed := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), seed, SessionBinding{})
	require.NoError(t, err)
	submitCtx, cancel := context.WithCancel(t.Context())
	submission, err := handle.Submit(submitCtx, TurnInput{Content: "independent execution"})
	require.NoError(t, err)
	cancel()
	require.NoError(t, handle.AwaitTurn(t.Context(), submission.TurnID))
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, snapshot.GetLastAssistantMessageContent())
}

func TestOfficialToolRecallRemainsAddressedWithTwoRoots(t *testing.T) {
	const secret = "origin-root-only recall"
	toolset := []tools.Tool{{Name: "recall", Parameters: map[string]any{}, Handler: func(ctx context.Context, _ tools.ToolCall, worker tools.Runtime) (*tools.ToolCallResult, error) {
		return tools.ResultSuccess("accepted"), worker.Recall(ctx, secret)
	}}}
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithToolSets(newStubToolSet(nil, toolset, nil)))
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root)), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	first, err := rt.CreateSession(t.Context(), session.New(session.WithToolsApproved(true)), SessionBinding{})
	require.NoError(t, err)
	second, err := rt.CreateSession(t.Context(), session.New(session.WithToolsApproved(true)), SessionBinding{})
	require.NoError(t, err)
	origin := first.(*sessionHandle).driver
	other := second.(*sessionHandle).driver
	for _, driver := range []*sessionDriver{origin, other} {
		require.NoError(t, driver.ownerCall(t.Context(), func() error { driver.phase = sessionRunning; return nil }))
	}
	scratch, err := origin.ownerSnapshot(t.Context())
	require.NoError(t, err)
	stopped, message := rt.processToolCalls(t.Context(), scratch, []tools.ToolCall{{ID: "recall-origin", Function: tools.FunctionCall{Name: "recall", Arguments: "{}"}}}, toolset, &collectSink{})
	assert.False(t, stopped)
	assert.Empty(t, message)
	otherSnapshot, err := other.ownerSnapshot(t.Context())
	require.NoError(t, err)
	otherResult := rt.drainAndEmitSteered(t.Context(), otherSnapshot, root, &collectSink{})
	assert.False(t, otherResult.drained)
	for _, item := range otherSnapshot.Messages {
		if item.Message != nil {
			assert.NotContains(t, item.Message.Message.Content, secret)
		}
	}
	batch := origin.drainBoundarySteering()
	require.Len(t, batch, 1)
	assert.Equal(t, secret, batch[0].Content)
	require.Error(t, rt.Steer(t.Context(), QueuedMessage{Content: "ambiguous"}))
}

func TestExecutionRefreshPreservesLegacyBlanketApproval(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name()), session.WithToolsApproved(true)))
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.generation = 1; return nil }))
	ctx, scratch, err := d.executionContext(t.Context(), 1)
	require.NoError(t, err)
	require.NoError(t, refreshExecutionInput(ctx, scratch))
	approved, _, _ := scratch.SafetySettings()
	assert.True(t, approved)
}

func TestExecutionRefreshPromotesAlreadySnapshottedSteering(t *testing.T) {
	r := newDriverTestRuntime(t)
	d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
	require.NoError(t, r.Steer(t.Context(), QueuedMessage{Content: "idle guidance", RequestID: "guidance"}))
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.generation = 1; return nil }))
	ctx, scratch, err := d.executionContext(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, scratch.Messages, 1)
	require.True(t, scratch.Messages[0].Message.Pending)
	require.Len(t, d.DrainSteering(), 1)
	require.NoError(t, refreshExecutionInput(ctx, scratch))
	require.Len(t, scratch.Messages, 1, "promotion must not duplicate the input")
	assert.False(t, scratch.Messages[0].Message.Pending, "provider scratch must consume owner-promoted guidance")
}

func TestOwnerElicitationAbandonHasOneTerminalWinner(t *testing.T) {
	for range 32 {
		r := newDriverTestRuntime(t)
		d := r.sessionDrivers.Get(session.New(session.WithID(t.Name())))
		waiter := newElicitationWaiter()
		event := ElicitationRequest("question", "form", nil, "", "question", "", d.identityID, nil, "root").(*ElicitationRequestEvent)
		event.RequestID = "question"
		require.NoError(t, d.registerElicitation(t.Context(), "question", event, waiter))
		d.abandonElicitation("question", newElicitationWaiter())
		var workers sync.WaitGroup
		workers.Go(func() { d.abandonElicitation("question", waiter) })
		workers.Go(func() {
			_ = d.Respond(InteractionResponse{InteractionID: "question", Kind: InteractionElicitation, ElicitationID: "question", Elicitation: ElicitationResult{Action: tools.ElicitationActionAccept}})
		})
		workers.Go(func() {
			assert.NoError(t, d.ownerCall(t.Context(), func() error { d.resolveInteractionsLocked(); return nil }))
		})
		workers.Wait()
		d.abandonElicitation("question", waiter)
		require.NoError(t, d.ownerCall(t.Context(), func() error { d.stopped = true; d.resolveInteractionsLocked(); return nil }))
		zero := uint64(0)
		replay, _, cancel, _ := d.events.SubscribeSequenced(d.identityID, &zero, 8)
		cancel()
		resolutions := 0
		for _, envelope := range replay {
			if _, ok := envelope.Event.(*InteractionResolvedEvent); ok {
				resolutions++
			}
		}
		assert.Equal(t, 1, resolutions)
	}
}
