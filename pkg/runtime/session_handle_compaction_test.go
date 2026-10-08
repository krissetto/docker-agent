package runtime

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestCompactorSettledSessionEmitsCanonicalEvents(t *testing.T) {
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{{
		stream: newStreamBuilder().AddContent("session summary").AddStopWithUsage(10, 5).Build(),
	}}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("session-compact")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	compactor := handle

	events := make(chan Event, 16)
	require.NoError(t, compactor.Compact(t.Context(), "focus", NewChannelSink(events)))
	handle.(*sessionHandle).driver.Wait()
	close(events)

	var statuses []string
	for event := range events {
		if compact, ok := event.(*SessionCompactionEvent); ok {
			statuses = append(statuses, compact.Status+":"+compact.Outcome)
		}
	}
	assert.Equal(t, []string{"started:", "completed:" + CompactionOutcomeApplied}, statuses)
	assert.Equal(t, "session summary", sessionHandleSnapshot(t, handle).LastSummary())
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SessionStateSettled, status.State)
}

func TestRunningSubmitReportsQueuedDisposition(t *testing.T) {
	rt := newLiveSessionsRuntime(t, &stepProvider{id: "test/mock-model"}, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("session-running-submit")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	local := handle.(*sessionHandle)
	local.driver.mu.Lock()
	local.driver.phase = sessionRunning
	local.driver.mu.Unlock()

	submission, err := handle.Submit(t.Context(), TurnInput{Content: "next"})
	require.NoError(t, err)
	assert.Equal(t, SubmissionDispositionQueued, submission.Disposition)

	func() {
		local.driver.mu.Lock()
		defer local.driver.mu.Unlock()
		local.driver.leave(sessionRunning)
		local.driver.pending = nil
	}()
}

func TestCompactorRunningSessionUsesBoundaryQueue(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		{stream: newStreamBuilder().AddToolCallName("call", "unknown_tool").AddToolCallArguments("call", "{}").AddToolCallStopWithUsage(1, 1).Build(), started: started, release: release},
		{stream: newStreamBuilder().AddContent("boundary summary").AddStopWithUsage(10, 5).Build()},
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("session-live-compact")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	stream := rt.runExecution(t.Context(), handle.(*sessionHandle).driver.session())
	waitClosed(t, started, "session turn")

	events := make(chan Event, 16)
	require.NoError(t, handle.Compact(t.Context(), "focus", NewChannelSink(events)))
	err = handle.Compact(t.Context(), "again", nil)
	var busyErr *SessionError
	require.ErrorAs(t, err, &busyErr)
	assert.Equal(t, SessionOperationCompactBusy, busyErr.Operation)
	close(release)
	drainStream(t, stream)
	close(events)

	var completed bool
	for event := range events {
		if compact, ok := event.(*SessionCompactionEvent); ok && compact.Status == "completed" {
			completed = true
		}
	}
	assert.True(t, completed)
	assert.Equal(t, "boundary summary", sessionHandleSnapshot(t, handle).LastSummary())
}

func TestCompactorRunningRejectsExistingPendingAndSteering(t *testing.T) {
	rt := newLiveSessionsRuntime(t, &stepProvider{id: "test/mock-model"}, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("session-running-pending")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	d := handle.(*sessionHandle).driver
	d.mu.Lock()
	d.phase = sessionRunning
	d.pending = []QueuedMessage{{Content: "later", RequestID: "pending"}}
	d.steering = []QueuedMessage{{Content: "guide", RequestID: "steer"}}
	d.mu.Unlock()

	err = handle.Compact(t.Context(), "", nil)
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionOperationCompactPending, sessionErr.Operation)
	d.mu.Lock()
	defer d.mu.Unlock()
	assert.False(t, d.compactReserved)
	assert.Len(t, d.pending, 1)
	assert.Len(t, d.steering, 1)
	d.leave(sessionRunning)
}

func TestCompactorReservationAcceptsSubmitAndSteerIntoPendingFIFO(t *testing.T) {
	turnStarted := make(chan struct{})
	releaseTurn := make(chan struct{})
	compactStarted := make(chan struct{})
	releaseCompact := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		{stream: newStreamBuilder().AddToolCallName("call", "boundary_tool").AddToolCallArguments("call", "{}").AddToolCallStopWithUsage(1, 1).Build()},
		{stream: newStreamBuilder().AddContent("reserved summary").AddStopWithUsage(10, 5).Build(), started: compactStarted, release: releaseCompact},
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()},
	}}
	boundaryTool := tools.Tool{Name: "boundary_tool", Parameters: map[string]any{}, Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
		close(turnStarted)
		select {
		case <-releaseTurn:
			return tools.ResultSuccess("paired boundary result"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000}, agent.WithToolSets(newStubToolSet(nil, []tools.Tool{boundaryTool}, nil)))
	sess := newWorkerSession("session-reserved")
	sess.ToolsApproved = true
	// Force compaction to summarize old bulk while retaining the recent
	// assistant/tool boundary tail verbatim.
	sess.AddMessage(session.UserMessage(strings.Repeat("old context ", 20_000)))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	stream := rt.runExecution(t.Context(), handle.(*sessionHandle).driver.session())
	waitClosed(t, turnStarted, "committed tool execution")
	require.NoError(t, handle.Compact(t.Context(), "", nil))

	// Admit while the active turn is still in flight, before the live loop can
	// reach the reserved compaction boundary and capture its snapshot.
	submit, submitErr := handle.Submit(t.Context(), TurnInput{Content: "later"})
	steer, steerErr := handle.Steer(t.Context(), TurnInput{Content: "guide"})
	require.NoError(t, submitErr)
	require.NoError(t, steerErr)
	assert.Equal(t, SubmissionDispositionQueued, submit.Disposition)
	assert.Equal(t, SubmissionDispositionQueued, steer.Disposition)

	d := handle.(*sessionHandle).driver
	d.mu.Lock()
	require.Len(t, d.pending, 2)
	assert.Equal(t, []string{"later", "guide"}, []string{d.pending[0].Content, d.pending[1].Content})
	assert.Empty(t, d.steering, "reserved steer must not cross the compaction barrier")
	d.mu.Unlock()

	close(releaseTurn)
	waitClosed(t, compactStarted, "boundary compaction")
	prov.mu.Lock()
	require.GreaterOrEqual(t, len(prov.messages), 2)
	compactInput := append([]chat.Message(nil), prov.messages[1]...)
	prov.mu.Unlock()
	for _, message := range compactInput {
		assert.NotContains(t, message.Content, "later")
		assert.NotContains(t, message.Content, "guide")
	}
	assert.NotContains(t, sessionHandleSnapshot(t, handle).LastSummary(), "later")
	assert.NotContains(t, sessionHandleSnapshot(t, handle).LastSummary(), "guide")

	close(releaseCompact)
	drainStream(t, stream)
	assert.Equal(t, "reserved summary", sessionHandleSnapshot(t, handle).LastSummary())
	assert.False(t, d.compactReserved)
	prov.mu.Lock()
	require.GreaterOrEqual(t, len(prov.messages), 3)
	nextInputs := make([][]chat.Message, len(prov.messages)-2)
	for i := 2; i < len(prov.messages); i++ {
		nextInputs[i-2] = append([]chat.Message(nil), prov.messages[i]...)
	}
	prov.mu.Unlock()
	var sawToolTail, sawLater, sawGuide bool
	for _, input := range nextInputs {
		for _, message := range input {
			sawToolTail = sawToolTail || len(message.ToolCalls) != 0 || message.Role == chat.MessageRoleTool
			sawLater = sawLater || strings.Contains(message.Content, "later")
			sawGuide = sawGuide || strings.Contains(message.Content, "guide")
		}
	}
	assert.True(t, sawToolTail, "assistant/tool tail appended after pending admission remains in next context")
	assert.True(t, sawLater)
	assert.True(t, sawGuide)
}

func TestSteerAfterCompactionReleaseStaysBehindPendingFIFO(t *testing.T) {
	rt := newLiveSessionsRuntime(t, &stepProvider{id: "test/mock-model"}, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("session-post-compact-boundary")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	local := handle.(*sessionHandle)

	local.driver.mu.Lock()
	local.driver.phase = sessionRunning
	first := QueuedMessage{Content: "accepted during compact", RequestID: "first"}
	local.driver.compactReserved = false // reservation just released at the loop boundary
	local.driver.mu.Unlock()
	_, err = local.driver.admitInput(t.Context(), first, SessionOperationPost, true, false)
	require.NoError(t, err)

	submission, err := handle.Steer(t.Context(), TurnInput{Content: "newer steer"})
	require.NoError(t, err)
	assert.Equal(t, SubmissionDispositionQueued, submission.Disposition)
	local.driver.mu.Lock()
	require.Len(t, local.driver.pending, 2)
	assert.Equal(t, []string{"accepted during compact", "newer steer"}, []string{local.driver.pending[0].Content, local.driver.pending[1].Content})
	assert.Empty(t, local.driver.steering)
	local.driver.mu.Unlock()

	local.driver.mu.Lock()
	local.driver.pending = make([]QueuedMessage, local.driver.pendingLimit())
	local.driver.mu.Unlock()
	_, err = handle.Steer(t.Context(), TurnInput{Content: "overflow"})
	var capacity *SessionError
	require.ErrorAs(t, err, &capacity)
	assert.Equal(t, SessionErrorCapacity, capacity.Kind)
	assert.Equal(t, local.driver.pendingLimit(), capacity.Limit)

	func() {
		local.driver.mu.Lock()
		defer local.driver.mu.Unlock()
		local.driver.pending = nil
		local.driver.leave(sessionRunning)
	}()
}

func TestCompactorStandaloneQueuesCrossModeFIFOAndWakesPending(t *testing.T) {
	compactStarted := make(chan struct{})
	releaseCompact := make(chan struct{})
	turnStarted := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		{stream: newStreamBuilder().AddContent("standalone summary").AddStopWithUsage(10, 5).Build(), started: compactStarted, release: releaseCompact},
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build(), started: turnStarted},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("session-standalone-queued")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	local := handle.(*sessionHandle)
	require.NoError(t, local.Compact(t.Context(), "", nil))
	waitClosed(t, compactStarted, "standalone compaction")

	steer, err := handle.Steer(t.Context(), TurnInput{Content: "first"})
	require.NoError(t, err)
	submit, err := handle.Submit(t.Context(), TurnInput{Content: "second"})
	require.NoError(t, err)
	assert.Equal(t, SubmissionDispositionQueued, steer.Disposition)
	assert.Equal(t, SubmissionDispositionQueued, submit.Disposition)
	local.driver.mu.Lock()
	require.Len(t, local.driver.pending, 2)
	assert.Equal(t, []string{"first", "second"}, []string{local.driver.pending[0].Content, local.driver.pending[1].Content})
	assert.Empty(t, local.driver.steering)
	local.driver.mu.Unlock()

	close(releaseCompact)
	waitClosed(t, turnStarted, "queued work wake")
	local.driver.Wait()
	assert.Equal(t, "standalone summary", sessionHandleSnapshot(t, handle).LastSummary())
	prov.mu.Lock()
	require.GreaterOrEqual(t, len(prov.messages), 2)
	compactInput := prov.messages[0]
	prov.mu.Unlock()
	for _, message := range compactInput {
		assert.NotContains(t, message.Content, "first")
		assert.NotContains(t, message.Content, "second")
	}
}

func TestCompactorStandaloneStopsAndWaitsWithoutMutation(t *testing.T) {
	started := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{{
		stream:  newStreamBuilder().AddContent("must not apply").AddStopWithUsage(10, 5).Build(),
		started: started,
		release: make(chan struct{}),
	}}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("session-stop-compact")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	local := handle.(*sessionHandle)
	require.NoError(t, local.Compact(t.Context(), "", nil))
	waitClosed(t, started, "standalone compaction model call")

	assert.True(t, local.driver.StopAll(), "standalone compaction retains a cancellable operation")
	done := make(chan struct{})
	go func() { local.driver.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("driver did not wait for cancelled standalone compaction")
	}
	assert.Empty(t, sessionHandleSnapshot(t, handle).LastSummary())
	assert.True(t, local.driver.isStopped())
}

func TestCompactorStoppedDuringCancellationCleanupRejectsInputAsStopped(t *testing.T) {
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{{
		stream: newStreamBuilder().AddContent("summary").AddStopWithUsage(10, 5).Build(),
	}}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("session-stop-cleanup")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	local := handle.(*sessionHandle)
	cleanupHeld := make(chan struct{})
	releaseCleanup := make(chan struct{})
	sink := EventSinkFunc(func(Event) {
		select {
		case <-cleanupHeld:
		default:
			close(cleanupHeld)
		}
		<-releaseCleanup
	})
	require.NoError(t, local.Compact(t.Context(), "", sink))
	waitClosed(t, cleanupHeld, "compaction cleanup sink")
	assert.True(t, local.driver.StopAll())
	before := len(sessionHandleSnapshot(t, handle).MessagesSnapshot())

	_, submitErr := handle.Submit(t.Context(), TurnInput{Content: "later"})
	_, steerErr := handle.Steer(t.Context(), TurnInput{Content: "guide"})
	for _, err := range []error{submitErr, steerErr} {
		var sessionErr *SessionError
		require.ErrorAs(t, err, &sessionErr)
		assert.Equal(t, SessionErrorStopped, sessionErr.Kind)
	}
	assert.Len(t, sessionHandleSnapshot(t, handle).MessagesSnapshot(), before)
	local.driver.mu.Lock()
	assert.True(t, local.driver.compactReserved, "reservation remains through terminal cleanup")
	local.driver.mu.Unlock()

	close(releaseCleanup)
	local.driver.Wait()
}

func TestCompactorIdleHistoryRetainsDurablePendingFIFO(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store session.Store = session.NewInMemorySessionStore()
			if backend == "sqlite" {
				store = coordinationSQLite(t)
			}
			compactStarted, releaseCompact := make(chan struct{}), make(chan struct{})
			prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
				{stream: newStreamBuilder().AddContent("history summary").AddStopWithUsage(10, 5).Build(), started: compactStarted, release: releaseCompact},
			}}
			rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})
			rt.sessionStore = store
			sess := session.New(session.WithID("idle-history"), session.WithAgentName("worker"), session.WithUserMessage("old history"))
			handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
			require.NoError(t, err)
			d := handle.(*sessionHandle).driver
			d.SetPreStartGate(func() bool { return false }, nil)
			inputs := []QueuedMessage{
				{Content: "old queued user", RequestID: "first"},
				{Content: "old child report", RequestID: "second", InputOrigin: session.InputOriginAgent, SenderID: "child", SenderName: "worker", ReportOutcome: session.ReportOutcomeFinished},
			}
			for _, input := range inputs {
				_, err := d.admitInput(t.Context(), input, SessionOperationPost, true, false)
				require.NoError(t, err)
			}
			require.NoError(t, handle.Compact(t.Context(), "", nil))
			waitClosed(t, compactStarted, "idle history compaction")
			d.SetPreStartErrorGate(nil, nil)
			submit, err := handle.Submit(t.Context(), TurnInput{Content: "new queued user", RequestID: "third"})
			require.NoError(t, err)
			steer, err := handle.Steer(t.Context(), TurnInput{Content: "new queued steer", RequestID: "fourth"})
			require.NoError(t, err)
			require.NoError(t, d.ownerCall(t.Context(), func() error {
				assert.Equal(t, []string{"first", "second", submit.TurnID, steer.TurnID}, pendingRequestIDs(d.pending))
				assert.Empty(t, d.steering)
				assert.True(t, d.compactReserved)
				return nil
			}))
			stored, err := store.GetSession(t.Context(), handle.ID())
			require.NoError(t, err)
			assert.Equal(t, []string{"first", "second", submit.TurnID, steer.TurnID}, pendingTurnIDs(stored))
			prov.mu.Lock()
			require.Len(t, prov.messages, 1, "pending work cannot start through reservation")
			compactInput := slices.Clone(prov.messages[0])
			prov.mu.Unlock()
			for _, message := range compactInput {
				for _, text := range []string{"old queued user", "old child report", "new queued user", "new queued steer"} {
					assert.NotContains(t, message.Content, text)
				}
			}
			close(releaseCompact)
			d.Wait()
			snapshot := sessionHandleSnapshot(t, handle)
			assert.Equal(t, "history summary", snapshot.LastSummary())
			assert.Empty(t, pendingTurnIDs(snapshot))
			stored, err = store.GetSession(t.Context(), handle.ID())
			require.NoError(t, err)
			assert.Equal(t, "history summary", stored.LastSummary())
			assert.Empty(t, pendingTurnIDs(stored))
			var promoted []string
			for _, event := range canonicalReplay(d) {
				if input, ok := event.Event.(*PendingUserMessagePromotedEvent); ok {
					promoted = append(promoted, input.TurnID)
				}
			}
			assert.Equal(t, []string{"first", "second", submit.TurnID, steer.TurnID}, promoted)
			prov.mu.Lock()
			defer prov.mu.Unlock()
			for _, text := range []string{"old queued user", "old child report", "new queued user", "new queued steer"} {
				found := false
				for _, call := range prov.messages[1:] {
					for _, message := range call {
						found = found || strings.Contains(message.Content, text)
					}
				}
				assert.True(t, found, "promoted input %q must survive the summary boundary", text)
			}
		})
	}
}

func TestCompactorDormantHistoryRetainsPendingWithoutWake(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{{
		stream: newStreamBuilder().AddContent("dormant summary").AddStopWithUsage(10, 5).Build(), started: started, release: release,
	}}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})
	sess := session.New(session.WithID("dormant-history"), session.WithAgentName("worker"), session.WithUserMessage("old history"))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	d := handle.(*sessionHandle).driver
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.viewDormant = true; return nil }))
	input := QueuedMessage{Content: "pending child delivery", RequestID: "delivery", InputOrigin: session.InputOriginAgent, SenderID: "child", SenderName: "worker", InputMode: "steer"}
	require.True(t, d.postTrustedInput(t.Context(), input))
	require.NoError(t, handle.Compact(t.Context(), "", nil))
	waitClosed(t, started, "dormant history compaction")
	close(release)
	d.Wait()
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		assert.True(t, d.viewDormant)
		assert.Equal(t, []string{"delivery"}, pendingRequestIDs(d.pending))
		assert.False(t, d.compactReserved)
		assert.Equal(t, sessionIdle, d.phase)
		return nil
	}))
	snapshot := sessionHandleSnapshot(t, handle)
	assert.Equal(t, "dormant summary", snapshot.LastSummary())
	assert.Equal(t, []string{"delivery"}, pendingTurnIDs(snapshot))
	prov.mu.Lock()
	defer prov.mu.Unlock()
	assert.Len(t, prov.messages, 1, "compaction must not authorize dormant work")
}

func pendingRequestIDs(inputs []QueuedMessage) []string {
	var ids []string
	for _, input := range inputs {
		ids = append(ids, input.RequestID)
	}
	return ids
}

func pendingTurnIDs(sess *session.Session) []string {
	var ids []string
	for _, item := range sess.MessagesSnapshot() {
		if item.Message != nil && item.Message.Pending {
			ids = append(ids, item.Message.TurnID)
		}
	}
	return ids
}

func TestCompactorIdleRootRetainsReportFromRunningChild(t *testing.T) {
	compactStarted, releaseCompact := make(chan struct{}), make(chan struct{})
	childStarted, releaseChild := make(chan struct{}), make(chan struct{})
	rootProv := &stepProvider{id: "test/coordination", steps: []providerStep{{
		stream: newStreamBuilder().AddContent("root history summary").AddStopWithUsage(10, 5).Build(), started: compactStarted, release: releaseCompact,
	}}}
	childProv := &stepProvider{id: "test/coordination", steps: []providerStep{{
		stream: newStreamBuilder().AddContent("arriving child report").AddStopWithUsage(1, 1).Build(), started: childStarted, release: releaseChild,
	}}}
	adapt := func(prov *stepProvider) *coordinationProvider {
		return &coordinationProvider{mockProvider: &mockProvider{id: prov.id}, call: func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
			return prov.CreateChatCompletionStream(ctx, messages, []tools.Tool{})
		}}
	}
	store := coordinationSQLite(t)
	_, owner := coordinationRuntime(t, store, adapt(rootProv), adapt(childProv), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
	root := coordinationCreate(t, owner.Runtime(), "compact-running-child-root", "")
	d := root.(*sessionHandle).driver
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.sess.AddMessage(session.UserMessage("settled root history")); return nil }))
	d.SetPreStartGate(func() bool { return false }, nil)
	child := coordinationCreate(t, owner.Runtime(), "compact-running-child", root.ID())
	submitted, err := child.Submit(t.Context(), TurnInput{Content: "child task"})
	require.NoError(t, err)
	waitClosed(t, childStarted, "running descendant")
	require.True(t, d.postTrustedInput(t.Context(), QueuedMessage{Content: "older child guidance", RequestID: "older", InputOrigin: session.InputOriginAgent, SenderID: child.ID(), SenderName: "worker", InputMode: "steer"}))
	require.NoError(t, root.Compact(t.Context(), "", nil))
	waitClosed(t, compactStarted, "root history compaction")
	childStatus, err := child.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SessionStateRunning, childStatus.State)
	close(releaseChild)
	coordinationAwait(t, child, submitted.TurnID)
	require.Eventually(t, func() bool {
		status, err := root.Status(t.Context())
		return err == nil && status.Pending >= 2
	}, time.Second, time.Millisecond, "child report delivery")
	var pending []QueuedMessage
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		pending = slices.Clone(d.pending)
		assert.True(t, d.compactReserved)
		assert.False(t, d.viewDormant)
		return nil
	}))
	require.GreaterOrEqual(t, len(pending), 2, "child completion must arrive through the reserved root mailbox")
	assert.Equal(t, "older", pending[0].RequestID)
	var report bool
	for _, input := range pending[1:] {
		report = report || strings.Contains(input.Content, "arriving child report")
	}
	assert.True(t, report)
	close(releaseCompact)
	d.Wait()
	stored, err := store.GetSession(t.Context(), root.ID())
	require.NoError(t, err)
	assert.Equal(t, "root history summary", stored.LastSummary())
	assert.Equal(t, pendingRequestIDs(pending), pendingTurnIDs(stored))
	rootProv.mu.Lock()
	defer rootProv.mu.Unlock()
	require.Len(t, rootProv.messages, 1)
	for _, message := range rootProv.messages[0] {
		assert.NotContains(t, message.Content, "older child guidance")
		assert.NotContains(t, message.Content, "arriving child report")
	}
}
