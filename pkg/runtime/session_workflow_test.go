package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
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
