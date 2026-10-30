package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	shelltool "github.com/docker/docker-agent/pkg/tools/builtin/shell"
	"github.com/docker/docker-agent/pkg/tools/builtin/shellprocess"
)

func TestAwaitWorkflowIncludesNestedChildrenAndRootFollowUp(t *testing.T) {
	leafEntered, leafRelease, rootEntered, rootRelease := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	rootModel := coordinationReply("final")
	rootModel.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		if calls.Add(1) == 1 {
			close(rootEntered)
			select {
			case <-rootRelease:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return newStreamBuilder().AddContent("final").AddStopWithUsage(1, 1).Build(), nil
	}
	workerModel := coordinationReply("worker final")
	workerModel.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
		for _, message := range messages {
			if message.Content == "leaf work" {
				close(leafEntered)
				select {
				case <-leafRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				break
			}
		}
		return newStreamBuilder().AddContent("worker final").AddStopWithUsage(1, 1).Build(), nil
	}
	rt, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "root", agent.WithModel(rootModel), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "worker", agent.WithModel(workerModel), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
	)), WithSessionStore(session.NewInMemorySessionStore()), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	root := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", root.ID())
	leaf := coordinationCreate(t, owner.Runtime(), "leaf", child.ID())
	_, err = leaf.Submit(t.Context(), TurnInput{Content: "leaf work"})
	require.NoError(t, err)
	coordinationWait(t, leafEntered)
	done := make(chan error, 1)
	go func() { _, err := root.(SessionWorkflow).AwaitWorkflow(t.Context()); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("returned before nested leaf finished: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(leafRelease)
	coordinationWait(t, rootEntered)
	select {
	case err := <-done:
		t.Fatalf("returned before root follow-up finished: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(rootRelease)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("workflow did not settle")
	}
	require.False(t, rt.HasRunningSubagents(root.ID()))
	// Retained idle descendants are reusable, not outstanding workflow work.
	_, err = root.(SessionWorkflow).AwaitWorkflow(t.Context())
	require.NoError(t, err)
}

type workflowReportStore struct {
	session.Store
	session.CoordinationStore
	acknowledged chan struct{}
	release      chan struct{}
	pollErr      error
}

func (s *workflowReportStore) PendingReports(ctx context.Context, parent string) ([]session.ChildReport, error) {
	if s.pollErr != nil {
		return nil, s.pollErr
	}
	return s.CoordinationStore.PendingReports(ctx, parent)
}

func (s *workflowReportStore) AcceptReport(ctx context.Context, parent, id string, message *session.Message) (session.ReportAcceptance, error) {
	accepted, err := s.CoordinationStore.AcceptReport(ctx, parent, id, message)
	if err == nil && accepted.Created {
		close(s.acknowledged)
		select {
		case <-s.release:
		case <-ctx.Done():
			return accepted, ctx.Err()
		}
	}
	return accepted, err
}

func TestAwaitWorkflowBridgesAcknowledgedReportToOwnerAdmission(t *testing.T) {
	base := session.NewInMemorySessionStore()
	store := &workflowReportStore{Store: base, CoordinationStore: base.(session.CoordinationStore), acknowledged: make(chan struct{}), release: make(chan struct{})}
	_, owner := coordinationRuntime(t, store, coordinationReply("received"), coordinationReply("child complete"))
	root := coordinationCreate(t, owner.Runtime(), "root", "")
	child := coordinationCreate(t, owner.Runtime(), "child", root.ID())
	_, err := child.Submit(t.Context(), TurnInput{Content: "finish"})
	require.NoError(t, err)
	coordinationWait(t, store.acknowledged)
	reports, err := base.(session.CoordinationStore).PendingReports(t.Context(), root.ID())
	require.NoError(t, err)
	require.Empty(t, reports, "durable outbox already acknowledged")
	done := make(chan error, 1)
	go func() { _, err := root.(SessionWorkflow).AwaitWorkflow(t.Context()); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("returned before owner admission: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(store.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("workflow did not settle")
	}
	snapshot, err := root.Snapshot(t.Context())
	require.NoError(t, err)
	require.Contains(t, renderTranscript(snapshot, 0), "received")
}

func TestAwaitWorkflowReturnsStorageFailure(t *testing.T) {
	base := session.NewInMemorySessionStore()
	failure := errors.New("outbox unavailable")
	store := &workflowReportStore{Store: base, CoordinationStore: base.(session.CoordinationStore), pollErr: failure}
	_, owner := coordinationRuntime(t, store, coordinationReply("root"), coordinationReply("worker"))
	root := coordinationCreate(t, owner.Runtime(), "root", "")
	_, err := root.(SessionWorkflow).AwaitWorkflow(t.Context())
	require.ErrorIs(t, err, failure)
}

func TestWorkflowSettlementIncludesQueuedAndReservedWork(t *testing.T) {
	_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("root"), coordinationReply("worker"))
	handle := coordinationCreate(t, owner.Runtime(), "root", "").(*sessionHandle)
	for _, reservation := range []string{"pending", "io", "report retry", "skill", "settling"} {
		t.Run(reservation, func(t *testing.T) {
			require.NoError(t, handle.driver.ownerCall(t.Context(), func() error {
				switch reservation {
				case "pending":
					handle.driver.pending = []QueuedMessage{{RequestID: "queued"}}
				case "io":
					handle.driver.ioReservations = 1
				case "report retry":
					handle.driver.reportRetry = &session.ChildReport{ID: "retry"}
				case "skill":
					handle.driver.skillOperationID = "skill"
				case "settling":
					handle.driver.phase = sessionSettling
				}
				return nil
			}))
			_, quiet, err := handle.workflowSettlement(t.Context())
			require.NoError(t, err)
			require.False(t, quiet)
			require.NoError(t, handle.driver.ownerCall(t.Context(), func() error {
				handle.driver.pending = nil
				handle.driver.ioReservations, handle.driver.reportRetry = 0, nil
				handle.driver.skillOperationID, handle.driver.phase = "", sessionIdle
				return nil
			}))
		})
	}
}

func TestBackgroundWorkKeepsWorkflowOpenUntilFixedIDCompletionAdmission(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.subagents = newSubagentManager(r)
	sess := session.New(session.WithID("background-work"))
	d := r.sessionDrivers.Get(sess)
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.generation = 1; d.phase = sessionRunning; d.activeRequestID = "active"; return nil }))
	ctx := context.WithValue(t.Context(), executionIdentityKey{}, executionIdentity{driver: d, generation: 1})
	work, err := r.beginSessionBackgroundWork(ctx, sess, d)
	require.NoError(t, err)
	_, quiet, err := (&sessionHandle{runtime: r, driver: d, sessionID: sess.ID}).workflowSettlement(t.Context())
	require.NoError(t, err)
	require.False(t, quiet)
	guidance := "shell finished: Background shell command job_test completed.\n--- Untrusted command output ---\nuntrusted"
	require.NoError(t, work.Complete(guidance))
	firstID := work.(*sessionBackgroundWork).requestID
	require.NoError(t, work.Complete(guidance))
	require.Equal(t, firstID, work.(*sessionBackgroundWork).requestID)
	require.NoError(t, work.Close())
	require.NoError(t, work.Close())
	_, quiet, err = (&sessionHandle{runtime: r, driver: d, sessionID: sess.ID}).workflowSettlement(t.Context())
	require.NoError(t, err)
	require.False(t, quiet, "admitted completion remains workflow work in the steering mailbox")
	pending, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, pending.Messages, 1)
	require.Equal(t, firstID, pending.Messages[0].Message.TurnID)
	require.Equal(t, guidance, pending.Messages[0].Message.Message.Content)
}

func TestBackgroundWorkCancelSuppressesCompletionAndReleasesWorkflowCount(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.subagents = newSubagentManager(r)
	sess := session.New(session.WithID("background-work-cancel"))
	d := r.sessionDrivers.Get(sess)
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.generation = 1; d.phase = sessionRunning; d.activeRequestID = "active"; return nil }))
	ctx := context.WithValue(t.Context(), executionIdentityKey{}, executionIdentity{driver: d, generation: 1})
	work, err := r.beginSessionBackgroundWork(ctx, sess, d)
	require.NoError(t, err)
	canceled := make(chan struct{})
	var cancelSignal sync.Once
	work.(interface{ SetCancel(func()) }).SetCancel(func() { cancelSignal.Do(func() { close(canceled) }) })
	d.cancelBackgroundWorks(1)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("session cancellation did not cancel its adopted shell work")
	}
	work.Cancel()
	require.NoError(t, work.Complete("must not steer"))
	require.NoError(t, work.Close())
	snapshot, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.Messages)
	require.NoError(t, d.ownerCall(t.Context(), func() error { require.Zero(t, d.backgroundWorks); return nil }))
}

func TestBackgroundWorkRetriesBusyAdmissionWithStableRequestID(t *testing.T) {
	r := newDriverTestRuntime(t)
	r.subagents = newSubagentManager(r)
	sess := session.New(session.WithID("background-work-busy"))
	d := r.sessionDrivers.Get(sess)
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		d.generation = 1
		d.phase = sessionRunning
		d.activeRequestID = "active"
		d.skillOperationID = "busy"
		return nil
	}))
	ctx := context.WithValue(t.Context(), executionIdentityKey{}, executionIdentity{driver: d, generation: 1})
	work, err := r.beginSessionBackgroundWork(ctx, sess, d)
	require.NoError(t, err)
	id := work.(*sessionBackgroundWork).requestID
	done := make(chan error, 1)
	go func() { done <- work.Complete("completed output") }()
	select {
	case err := <-done:
		t.Fatalf("busy steer unexpectedly admitted: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	require.NoError(t, d.ownerCall(t.Context(), func() error { d.skillOperationID = ""; return nil }))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("completion admission was not retried")
	}
	require.Equal(t, id, work.(*sessionBackgroundWork).requestID)
	require.NoError(t, work.Close())
	snapshot, err := d.ownerSnapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.Messages, 1)
	require.Equal(t, id, snapshot.Messages[0].Message.TurnID)
}

func TestCancelActiveTurnWithoutBackgroundWorkDoesNotPanic(t *testing.T) {
	r := newDriverTestRuntime(t)
	sess := session.New(session.WithID("cancel-without-background-work"))
	d := r.sessionDrivers.Get(sess)
	require.NoError(t, d.ownerCall(t.Context(), func() error {
		d.phase = sessionRunning
		d.generation = 1
		d.activeRequestID = "active"
		d.cancel = func() {}
		return nil
	}))
	outcome, err := d.cancelTurn(t.Context(), "active")
	require.NoError(t, err)
	require.Equal(t, CancelAccepted, outcome)
}

func backgroundWorkExecution(t *testing.T, driver *sessionDriver) context.Context {
	t.Helper()
	var generation uint64
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		generation = driver.generation
		return nil
	}))
	ctx, _, err := driver.executionContext(t.Context(), generation)
	require.NoError(t, err)
	return ctx
}

func TestBackgroundWorkCompletionWakesRootExecutionAndKeepsWorkflowOpen(t *testing.T) {
	entered, release, completed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	root := coordinationReply("unused")
	root.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return newStreamBuilder().AddContent("initial").AddStopWithUsage(1, 1).Build(), nil
		}
		for _, message := range messages {
			if strings.Contains(message.Content, "captured shell output") {
				close(completed)
			}
		}
		return newStreamBuilder().AddContent("processed shell output").AddStopWithUsage(1, 1).Build(), nil
	}
	rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), root, coordinationReply("unused"))
	handle := coordinationCreate(t, owner.Runtime(), "background-root", "")
	submission, err := handle.Submit(t.Context(), TurnInput{Content: "start work"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	driver := handle.(*sessionHandle).driver
	work, err := rt.beginSessionBackgroundWork(backgroundWorkExecution(t, driver), driver.session(), driver)
	require.NoError(t, err)
	close(release)
	coordinationAwait(t, handle, submission.TurnID)
	settled := make(chan error, 1)
	go func() { _, err := handle.(SessionWorkflow).AwaitWorkflow(t.Context()); settled <- err }()
	select {
	case err := <-settled:
		t.Fatalf("workflow settled while shell work remained: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	const output = "Background shell command completed.\n--- Output ---\ncaptured shell output"
	require.NoError(t, work.Complete(output))
	workID := work.(*sessionBackgroundWork).requestID
	require.NoError(t, work.Close())
	coordinationWait(t, completed)
	coordinationAwait(t, handle, workID)
	select {
	case err := <-settled:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		driver.mu.Lock()
		deferred, background, pendingCount, phase := driver.deferredCompletionReportTurnID, driver.backgroundWorks, len(driver.pending), driver.phase
		driver.mu.Unlock()
		_, quiet, quietErr := handle.(*sessionHandle).workflowSettlement(t.Context())
		t.Fatalf("root workflow did not settle after shell completion wake: quiet=%v err=%v deferred=%q bg=%d pending=%d phase=%v status=%+v", quiet, quietErr, deferred, background, pendingCount, phase, driver.Status())
	}
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Contains(t, renderTranscript(snapshot, 0), "captured shell output")
}

type deferredCommitAckStore struct {
	session.Store
	session.CoordinationStore
	failNextReport atomic.Bool
}

func (s *deferredCommitAckStore) CommitChild(ctx context.Context, commit session.ChildCommit) error {
	err := s.CoordinationStore.CommitChild(ctx, commit)
	if err == nil && len(commit.Reports) != 0 && s.failNextReport.CompareAndSwap(true, false) {
		return session.ErrRevisionConflict
	}
	return err
}

func TestBackgroundWorkCompletionWakesChildBeforeParentReport(t *testing.T) {
	entered, release, processed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var workerCalls atomic.Int32
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
		if workerCalls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return newStreamBuilder().AddContent("initial child response").AddStopWithUsage(1, 1).Build(), nil
		}
		for _, message := range messages {
			if strings.Contains(message.Content, "--- Output ---") && strings.Contains(message.Content, "captured child shell output") {
				select {
				case <-processed:
				default:
					close(processed)
				}
			}
		}
		return newStreamBuilder().AddContent("child processed shell output").AddStopWithUsage(1, 1).Build(), nil
	}
	baseStore := session.NewInMemorySessionStore()
	store := &deferredCommitAckStore{Store: baseStore, CoordinationStore: baseStore.(session.CoordinationStore)}
	var parentCalls atomic.Int32
	parentModel := coordinationReply("parent received report")
	parentModel.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		parentCalls.Add(1)
		return newStreamBuilder().AddContent("parent processed deferred shell result").AddStopWithUsage(1, 1).Build(), nil
	}
	rt, owner := coordinationRuntime(t, store, parentModel, worker)
	root := coordinationCreate(t, owner.Runtime(), "background-child-root", "")
	child := coordinationCreate(t, owner.Runtime(), "background-child", root.ID())
	submission, err := child.Submit(t.Context(), TurnInput{Content: "start child work"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	driver := child.(*sessionHandle).driver
	work, err := rt.beginSessionBackgroundWork(backgroundWorkExecution(t, driver), driver.session(), driver)
	require.NoError(t, err)
	close(release)
	coordinationAwait(t, child, submission.TurnID)
	reports, err := store.CoordinationStore.PendingReports(t.Context(), root.ID())
	require.NoError(t, err)
	require.Empty(t, reports, "child must not report its first turn while retained shell work remains")
	const output = "Background shell command completed.\n--- Output ---\ncaptured child shell output"
	require.NoError(t, work.Complete(output))
	workID := work.(*sessionBackgroundWork).requestID
	coordinationWait(t, processed)
	coordinationAwait(t, child, workID)
	reports, err = store.CoordinationStore.PendingReports(t.Context(), root.ID())
	require.NoError(t, err)
	require.Empty(t, reports, "do not report while completion work is still registered")
	driver.mu.Lock()
	deferredID, background := driver.deferredCompletionReportTurnID, driver.backgroundWorks
	driver.mu.Unlock()
	require.Equal(t, workID, deferredID, "settlement with an owned process retains completion report intent")
	require.Equal(t, 1, background)
	rootDriver := root.(*sessionHandle).driver
	require.NoError(t, rootDriver.ownerCall(t.Context(), func() error { rootDriver.viewDormant = true; return nil }))
	store.failNextReport.Store(true)
	require.NoError(t, work.Close())
	require.Eventually(t, func() bool { return !store.failNextReport.Load() }, 5*time.Second, 10*time.Millisecond, "deferred report commit exercises the injected ambiguous acknowledgement")
	require.Eventually(t, func() bool { return !rootDriver.Status().Dormant && parentCalls.Load() == 1 }, 5*time.Second, 10*time.Millisecond, "the fresh deferred report authorizes a dormant parent wake")
	reportTurnID := "report:" + childReportID(child.ID(), workID)
	assert.Eventually(t, func() bool {
		snapshot, err := root.Snapshot(t.Context())
		if err != nil {
			return false
		}
		for _, item := range snapshot.MessagesSnapshot() {
			if item.Message != nil && item.Message.TurnID == reportTurnID {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "parent receives the deferred report after final background work closes")
	snapshot, err := root.Snapshot(t.Context())
	require.NoError(t, err)
	require.Contains(t, renderTranscript(snapshot, 0), "child processed shell output")
	require.Contains(t, renderTranscript(snapshot, 0), "parent processed deferred shell result")
}

func TestOrdinaryQueuedTurnDoesNotDeferEarlierChildReport(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var workerCalls atomic.Int32
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
		switch workerCalls.Add(1) {
		case 1:
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return newStreamBuilder().AddContent("first user turn finished").AddStopWithUsage(1, 1).Build(), nil
		default:
			return newStreamBuilder().AddContent("second user turn finished").AddStopWithUsage(1, 1).Build(), nil
		}
	}
	store := session.NewInMemorySessionStore()
	_, owner := coordinationRuntime(t, store, coordinationReply("root received report"), worker)
	root := coordinationCreate(t, owner.Runtime(), "ordinary-queued-root", "")
	child := coordinationCreate(t, owner.Runtime(), "ordinary-queued-child", root.ID())
	first, err := child.Submit(t.Context(), TurnInput{Content: "first", RequestID: "first"})
	require.NoError(t, err)
	coordinationWait(t, entered)
	second, err := child.Submit(t.Context(), TurnInput{Content: "second", RequestID: "second"})
	require.NoError(t, err)
	close(release)
	coordinationAwait(t, child, first.TurnID)
	firstReportTurnID := "report:" + childReportID(child.ID(), first.TurnID)
	assert.Eventually(t, func() bool {
		snapshot, err := root.Snapshot(t.Context())
		if err != nil {
			return false
		}
		for _, item := range snapshot.MessagesSnapshot() {
			if item.Message != nil && item.Message.TurnID == firstReportTurnID {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "ordinary queued user work does not suppress the completed turn report")
	coordinationAwait(t, child, second.TurnID)
}

func TestDeferredCompletionIntentBlocksReclaimAndWorkflowSettlement(t *testing.T) {
	rt := newDriverTestRuntime(t)
	rt.subagents = newSubagentManager(rt)
	sess := session.New(session.WithID("deferred-reclaim"))
	driver := rt.sessionDrivers.Get(sess)
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		driver.deferredCompletionReportTurnID = "completed-turn"
		return nil
	}))
	_, quiet, err := (&sessionHandle{runtime: rt, driver: driver, sessionID: sess.ID}).workflowSettlement(t.Context())
	require.NoError(t, err)
	require.False(t, quiet, "workflow settlement waits for deferred completion reporting")
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		require.NotEqual(t, driverReclaimed, driver.beginReclaimLocked(), "reclaim cannot pass the post-Close scheduling window")
		return nil
	}))
}

func TestDeferredCompletionIntentClearsWhenChildRecordIsStaleOrStopped(t *testing.T) {
	rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("root"), coordinationReply("worker"))
	root := coordinationCreate(t, owner.Runtime(), "stale-report-root", "")
	child := coordinationCreate(t, owner.Runtime(), "stale-report-child", root.ID())
	driver := child.(*sessionHandle).driver
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		driver.deferredCompletionReportTurnID = "superseded-turn"
		return nil
	}))
	require.False(t, rt.subagents.commitDeferredCompletionReport(driver))
	driver.mu.Lock()
	require.Empty(t, driver.deferredCompletionReportTurnID, "a deferred turn absent from the current child record is stale")
	driver.deferredCompletionReportTurnID = "stopped-turn"
	driver.mu.Unlock()
	rt.subagents.mu.Lock()
	for _, record := range rt.subagents.children {
		if record.sessionID == child.ID() {
			record.durable.Node.State = subagent.NodeStopped
		}
	}
	rt.subagents.mu.Unlock()
	require.False(t, rt.subagents.commitDeferredCompletionReport(driver))
	driver.mu.Lock()
	defer driver.mu.Unlock()
	require.Empty(t, driver.deferredCompletionReportTurnID, "a stopped child cannot retain a deferred report intent")
}

func TestShellBackgroundCompletionPrecedesSubagentReport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell command")
	}
	workingDir := t.TempDir()
	releasePath := filepath.Join(workingDir, "release-shell-job")
	release := func() { _ = os.WriteFile(releasePath, []byte("release"), 0o600) }
	t.Cleanup(release)
	runtimeConfig := &config.RuntimeConfig{Config: config.Config{WorkingDir: workingDir}}
	backgroundAfter := 1
	shellSet, err := shelltool.CreateToolSet(t.Context(), latest.Toolset{Type: "shell", BackgroundAfter: &backgroundAfter}, runtimeConfig)
	require.NoError(t, err)
	shellTools, ok := tools.As[*shelltool.ToolSet](shellSet)
	require.True(t, ok)
	jobs := shellprocess.New()
	shellTools.AttachProcessManager(jobs, true)

	var workerCalls, parentCalls atomic.Int32
	processedOutput := make(chan struct{})
	worker := coordinationReply("unused")
	worker.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
		call := workerCalls.Add(1)
		if call == 1 {
			command := fmt.Sprintf("while [ ! -f %q ]; do sleep 0.02; done; printf 'captured child shell output'", releasePath)
			return newStreamBuilder().AddToolCallName("background-shell", shelltool.ToolNameShell).AddToolCallArguments("background-shell", fmt.Sprintf(`{"cmd":%q}`, command)).AddToolCallStopWithUsage(1, 1).Build(), nil
		}
		for _, message := range messages {
			if strings.Contains(message.Content, "--- Untrusted command output ---") && strings.Contains(message.Content, "captured child shell output") {
				select {
				case <-processedOutput:
				default:
					close(processedOutput)
				}
			}
		}
		if call == 2 {
			return newStreamBuilder().AddContent("started the background shell command").AddStopWithUsage(1, 1).Build(), nil
		}
		return newStreamBuilder().AddContent("processed the shell output").AddStopWithUsage(1, 1).Build(), nil
	}
	root := coordinationReply("parent received completed shell result")
	root.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
		parentCalls.Add(1)
		return newStreamBuilder().AddContent("parent handled completed child result").AddStopWithUsage(1, 1).Build(), nil
	}
	tm := team.New(team.WithAgents(
		agent.New("root", "prompt", agent.WithModel(root), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "prompt", agent.WithModel(worker), agent.WithToolSets(shellTools)),
	))
	rt, err := NewLocalRuntime(t.Context(), tm, WithSessionStore(session.NewInMemorySessionStore()), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
	require.NoError(t, err)
	owner := NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	rootHandle, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("shell-report-root"), session.WithToolsApproved(true)), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	child, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("shell-report-child"), session.WithToolsApproved(true)), SessionBinding{AgentName: "worker", ParentSessionID: rootHandle.ID()})
	require.NoError(t, err)
	submission, err := child.Submit(t.Context(), TurnInput{Content: "run a background shell process"})
	require.NoError(t, err)
	coordinationAwait(t, child, submission.TurnID)
	driver := child.(*sessionHandle).driver
	if !assert.Eventually(t, func() bool {
		driver.mu.Lock()
		defer driver.mu.Unlock()
		return driver.backgroundWorks == 1
	}, 5*time.Second, 10*time.Millisecond) {
		state := driver.Status()
		snapshot, _ := child.Snapshot(t.Context())
		t.Fatalf("shell command did not remain owned: works=%d calls=%d status=%+v transcript=%s", driver.backgroundWorks, workerCalls.Load(), state, renderTranscript(snapshot, 0))
	}
	backgroundObservation, err := child.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer backgroundObservation.Cancel()
	require.Len(t, backgroundObservation.Primary().BackgroundProcesses, 1, "the adopted process remains visible after its tool response returns")
	process := backgroundObservation.Primary().BackgroundProcesses[0]
	processID := process.ID
	require.NotEmpty(t, processID)
	assert.Equal(t, "background-shell", process.ToolCallID, "tool-call identity must remain correlated after the shell response")
	assert.Equal(t, "worker", process.AgentName)
	assert.Equal(t, child.ID(), process.SessionID)
	// Observe lifecycle separately from tool-call transcript events: the process remains
	// live after the shell tool response has already returned.
	coord := rt.subagents.coordination()
	reports, err := coord.PendingReports(t.Context(), rootHandle.ID())
	require.NoError(t, err)
	require.Empty(t, reports, "the child must not publish its first-turn report before its shell command completes")
	assert.Zero(t, parentCalls.Load(), "the parent must not wake from a premature child report")
	parentSnapshot, err := rootHandle.Snapshot(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, renderTranscript(parentSnapshot, 0), "captured child shell output")

	release()
	coordinationWait(t, processedOutput)
	var terminalProcesses []tools.BackgroundProcess
	deadline := time.NewTimer(5 * time.Second)
	for terminalProcesses == nil {
		select {
		case envelope, open := <-backgroundObservation.Events:
			if !open {
				t.Fatal("background process observation closed before completion")
			}
			if event, ok := envelope.Event.(*BackgroundProcessesEvent); ok {
				for _, process := range event.Processes {
					if process.ID == processID && process.State == tools.BackgroundProcessCompleted {
						terminalProcesses = event.Processes
					}
				}
			}
		case <-deadline.C:
			t.Fatal("terminal process replacement was not delivered")
		}
	}
	deadline.Stop()
	backgroundObservation.Cancel()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = rootHandle.(SessionWorkflow).AwaitWorkflow(ctx)
	require.NoError(t, err)
	assert.Equal(t, int32(1), parentCalls.Load(), "the parent wakes exactly once, after completion output was processed")
	var terminalProcess tools.BackgroundProcess
	for _, process := range terminalProcesses {
		if process.ID == processID {
			terminalProcess = process
		}
	}
	require.Equal(t, processID, terminalProcess.ID)
	assert.Equal(t, tools.BackgroundProcessCompleted, terminalProcess.State)
	childSnapshot, err := child.Snapshot(t.Context())
	require.NoError(t, err)
	require.Contains(t, renderTranscript(childSnapshot, 0), "captured child shell output")
	parentSnapshot, err = rootHandle.Snapshot(t.Context())
	require.NoError(t, err)
	require.Contains(t, renderTranscript(parentSnapshot, 0), "processed the shell output")
}
