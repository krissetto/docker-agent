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

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func sessionHandleSnapshot(t *testing.T, handle SessionHandle) *session.Session {
	t.Helper()
	snapshot, err := handle.Snapshot(t.Context())
	require.NoError(t, err)
	return snapshot
}

func TestConcurrentIdleRetriesReportLoserQueued(t *testing.T) {
	started := make(chan struct{})
	releaseRun := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build(), started: started, release: releaseRun},
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("idle-retry-race")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	d := handle.(*sessionHandle).driver
	firstAppended := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	d.beforePrepareStart = func() {
		if calls.Add(1) == 1 {
			close(firstAppended)
			<-releaseFirst
		}
	}
	type result struct {
		submission Submission
		err        error
	}
	firstResult := make(chan result, 1)
	go func() {
		submission, err := handle.Retry(t.Context())
		firstResult <- result{submission, err}
	}()
	waitClosed(t, firstAppended, "first retry append")
	secondResult := make(chan result, 1)
	go func() {
		submission, err := handle.Retry(t.Context())
		secondResult <- result{submission, err}
	}()
	waitClosed(t, started, "second retry winning start")
	close(releaseFirst)
	first, second := <-firstResult, <-secondResult
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.Empty(t, first.submission.Disposition, "winner promoted first FIFO retry")
	assert.Equal(t, SubmissionDispositionQueued, second.submission.Disposition, "loser remains in mailbox")
	close(releaseRun)
	d.Wait()
}

func TestRetryGateFailureRemainsQueued(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	d := handle.(*sessionHandle).driver
	d.SetPreStartErrorGate(func() error { return retryable(assert.AnError) }, nil)

	submission, err := handle.Retry(t.Context())
	require.NoError(t, err)
	assert.Equal(t, SubmissionDispositionQueued, submission.Disposition)
	func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		require.Len(t, d.pending, 1)
		assert.True(t, d.pending[0].Retry)
		d.pending = nil
	}()
}

func TestRetryStopRaceReturnsErrorWhenMailboxIsCleared(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	d := handle.(*sessionHandle).driver
	// Fence scheduler starts too: the caller hook alone does not stop a wake.
	rt.sessionDrivers.runMu.Lock()
	unlockStarts := sync.OnceFunc(rt.sessionDrivers.runMu.Unlock)
	appended := make(chan struct{})
	release := make(chan struct{})
	releaseCaller := sync.OnceFunc(func() { close(release) })
	done := make(chan struct{})
	defer func() {
		d.StopAll()
		unlockStarts()
		releaseCaller()
		waitClosed(t, done, "stopped retry caller")
		d.Wait()
	}()
	d.beforePrepareStart = func() { close(appended); <-release }
	result := make(chan error, 1)
	go func() {
		defer close(done)
		_, err := handle.Retry(t.Context())
		result <- err
	}()
	waitClosed(t, appended, "retry mailbox append")
	func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		require.Len(t, d.pending, 1, "stop must precede retry promotion")
		assert.True(t, d.pending[0].Retry)
		assert.Zero(t, d.generation, "no start may cross the dispatch fence")
	}()
	d.StopAll()
	unlockStarts()
	releaseCaller()
	var stopped *SessionError
	require.ErrorAs(t, <-result, &stopped)
	assert.Equal(t, SessionErrorStopped, stopped.Kind, "cleared retry must report stopped rather than accepted or capacity-limited")
	require.ErrorIs(t, stopped, ErrSessionStopped)
	d.mu.Lock()
	defer d.mu.Unlock()
	assert.Empty(t, d.pending, "stopping clears accepted retry work")
	assert.False(t, d.running, "cleared retry must not start execution")
	assert.Zero(t, d.generation, "stopped retry must never create an execution generation")
}

func TestIdleConcurrentPostsStaySuccessfulWhenLaterCallerWinsStart(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseRun := make(chan struct{})
	prov := &stepProvider{id: "test/mock-model", steps: []providerStep{
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build(), started: firstStarted, release: releaseRun},
		{stream: newStreamBuilder().AddStopWithUsage(1, 1).Build()},
	}}
	rt := newLiveSessionsRuntime(t, prov, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("idle-post-race")
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	d := handle.(*sessionHandle).driver
	firstAppended := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	d.beforePrepareStart = func() {
		if calls.Add(1) == 1 {
			close(firstAppended)
			<-releaseFirst
		}
	}

	type result struct {
		submission Submission
		err        error
	}
	firstResult := make(chan result, 1)
	go func() {
		submission, err := handle.Submit(t.Context(), TurnInput{Content: "first"})
		firstResult <- result{submission: submission, err: err}
	}()
	waitClosed(t, firstAppended, "first durable append")
	secondResult := make(chan result, 1)
	go func() {
		submission, err := handle.Submit(t.Context(), TurnInput{Content: "second"})
		secondResult <- result{submission: submission, err: err}
	}()
	waitClosed(t, firstStarted, "later caller winning start")
	close(releaseFirst)

	first := <-firstResult
	second := <-secondResult
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	assert.NotEmpty(t, first.submission.TurnID)
	assert.NotEmpty(t, second.submission.TurnID)
	assert.NotEqual(t, first.submission.TurnID, second.submission.TurnID)
	assert.Empty(t, first.submission.Disposition, "first was already promoted by the competing start")
	assert.Equal(t, SubmissionDispositionQueued, second.submission.Disposition)

	close(releaseRun)
	d.Wait()
	prov.mu.Lock()
	inputs := append([][]chat.Message(nil), prov.messages...)
	prov.mu.Unlock()
	var order []string
	for _, input := range inputs {
		for _, message := range input {
			if message.Content == "first" || message.Content == "second" {
				order = append(order, message.Content)
			}
		}
	}
	assert.Equal(t, []string{"first", "first", "second"}, order, "first remains context for second; neither turn is duplicated")
}

func TestPromotionFailureDedupAllowsFailureClassTransition(t *testing.T) {
	rt, sess := newSessionFixture(t)
	d := rt.sessionDrivers.Get(sess)
	_, events, cancel := d.Subscribe(16)
	defer cancel()
	temporary := &session.TemporaryError{Err: errors.New("busy")}
	d.mu.Lock()
	d.publishPromotionFailureLocked("A", temporary)
	d.publishPromotionFailureLocked("A", temporary)
	d.publishPromotionFailureLocked("A", session.ErrNotFound)
	d.mu.Unlock()
	var count int
	for {
		select {
		case event := <-events:
			if _, ok := event.(*ErrorEvent); ok {
				count++
			}
		default:
			assert.Equal(t, 2, count, "identical transient noise dedups, permanent transition remains visible")
			return
		}
	}
}

func TestSteerReportsCanceledAndStoppedWithoutCapacityMasking(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = h.Steer(ctx, TurnInput{Content: "cancelled"})
	require.ErrorIs(t, err, context.Canceled)
	var sessionErr *SessionError
	assert.NotErrorAs(t, err, &sessionErr)

	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))
	_, err = h.Steer(t.Context(), TurnInput{Content: "stopped"})
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorStopped, sessionErr.Kind)
	assert.Equal(t, SessionOperationSteer, sessionErr.Operation)
	assert.Equal(t, sess.ID, sessionErr.SessionID)
	assert.NotEmpty(t, sessionErr.RequestID)
	assert.Zero(t, sessionErr.Limit)
	assert.ErrorIs(t, err, ErrSessionStopped)
}

func TestAcceptedBacklogRetriesAutonomouslyAndCorrelatesHeadFailure(t *testing.T) {
	rt, sess := newSessionFixture(t)
	// Seed restored-like accepted A before publishing its driver to the scheduler.
	a := session.UserMessage("A")
	a.Pending, a.Accepted, a.TurnID = true, true, "turn-a"
	sess.AddMessage(a)
	require.NoError(t, rt.sessionStore.AddSession(t.Context(), sess))
	reservation, err := rt.sessionDrivers.PrepareRestore(t.Context(), sess)
	require.NoError(t, err)
	defer reservation.Discard()
	d := reservation.driver
	var attempts int
	releaseRetry := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseRetry:
		default:
			close(releaseRetry)
		}
	})
	d.SetPreStartErrorGate(func() error {
		attempts++
		if attempts == 1 {
			return retryable(assert.AnError)
		}
		select {
		case <-releaseRetry:
			return nil
		case <-rt.lifetime().Done():
			return rt.lifetime().Err()
		}
	}, nil)
	require.NoError(t, rt.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, nil))
	h, err := rt.SessionByID(sess.ID)
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), ObserveOptions{Buffer: 64})
	require.NoError(t, err)
	defer obs.Cancel()

	b, err := h.Submit(t.Context(), TurnInput{Content: "B"})
	require.NoError(t, err, "B is accepted even though waking A fails")
	require.NotEmpty(t, b.TurnID)
	assert.Equal(t, SubmissionDispositionQueued, b.Disposition, "retained wake failure leaves B pending")
	close(releaseRetry)
	var failedTurn string
	require.Eventually(t, func() bool {
		select {
		case envelope := <-obs.Events:
			if _, ok := envelope.Event.(*ErrorEvent); ok {
				failedTurn = envelope.TurnID
			}
		default:
		}
		status, _ := h.Status(t.Context())
		return failedTurn != "" && status.State == SessionStateSettled
	}, 5*time.Second, time.Millisecond)
	assert.Equal(t, "turn-a", failedTurn, "wake failure belongs to FIFO head A, never newly accepted B")
	counts := map[string]int{}
	for _, message := range sessionHandleSnapshot(t, h).GetAllMessages() {
		if message.Message.Role == chat.MessageRoleUser {
			counts[message.Message.Content]++
		}
	}
	assert.Equal(t, 1, counts["A"])
	assert.Equal(t, 1, counts["B"])
}

func TestPermanentRetryFailureStopsWorkerAndRuntimeCancelWaits(t *testing.T) {
	rt, sess := newSessionFixture(t)
	reservation, err := rt.sessionDrivers.PrepareRestore(t.Context(), sess)
	require.NoError(t, err)
	defer reservation.Discard()
	d := reservation.driver
	d.pending = append(d.pending, QueuedMessage{Content: "A", RequestID: "A"})
	d.SetPreStartErrorGate(func() error {
		return &SessionError{Kind: SessionErrorUnsupported, SessionID: sess.ID, Operation: "gate"}
	}, nil)
	require.NoError(t, rt.sessionDrivers.ActivateRestoreBatch([]*restoreDriverReservation{reservation}, nil))
	d.schedulePendingRetry()
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return !d.retryRunning && len(d.pending) == 1
	}, time.Second, time.Millisecond)

	d.SetPreStartErrorGate(func() error { return retryable(assert.AnError) }, nil)
	d.schedulePendingRetry()
	rt.lifecycleCancel()
	done := make(chan struct{})
	go func() { d.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runtime cancellation must terminate retry worker and Wait")
	}
	select {
	case <-rt.sessionDrivers.workDone:
	case <-time.After(time.Second):
		t.Fatal("runtime cancellation must terminate the retry scheduler")
	}
}

func TestSessionHandlePinsSessionIdentity(t *testing.T) {
	rt := newLiveSessionsRuntime(t, &stepProvider{id: "test/mock-model"}, mockModelStoreWithLimit{limit: 100_000})
	sess := newWorkerSession("pinned-identity")
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	assert.Equal(t, sess.ID, h.ID())
	before := sessionHandleSnapshot(t, h)

	replacement := before.Clone()
	replacement.AddMessage(session.UserMessage("must not overwrite canonical transcript"))
	repeated, err := rt.CreateSession(t.Context(), replacement, SessionBinding{AgentName: "worker"})
	require.NoError(t, err)
	assert.Equal(t, h.ID(), repeated.ID())
	assert.Equal(t, h.AgentName(), repeated.AgentName())
	assert.Equal(t, before.MessagesSnapshot(), sessionHandleSnapshot(t, repeated).MessagesSnapshot())
	require.NoError(t, repeated.UpdateTitle(t.Context(), "shared canonical state"))
	assert.Equal(t, "shared canonical state", sessionHandleSnapshot(t, h).TitleSnapshot())

	_, err = rt.CreateSession(t.Context(), replacement, SessionBinding{AgentName: "root"})
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorInvalid, sessionErr.Kind)
	assert.Equal(t, before.MessagesSnapshot(), sessionHandleSnapshot(t, h).MessagesSnapshot())
}

func TestObservationCorrelatesSubmissionAndOrdersEvents(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()

	var previous uint64
	for _, content := range []string{"hello", "again"} {
		submission, err := h.Submit(t.Context(), TurnInput{Content: content})
		require.NoError(t, err)
		var terminal bool
		var stopped int
		for !terminal {
			select {
			case envelope, ok := <-obs.Events:
				require.True(t, ok, "observation closed after a settled turn")
				assert.Greater(t, envelope.Sequence, previous)
				previous = envelope.Sequence
				assert.Equal(t, submission.TurnID, envelope.TurnID)
				switch event := envelope.Event.(type) {
				case *StreamStoppedEvent:
					stopped++
				case *TurnSettledEvent:
					assert.Equal(t, 1, stopped, "stream stop precedes durable settlement exactly once")
					assert.Equal(t, h.ID(), event.SessionID)
					assert.Equal(t, submission.TurnID, event.TurnID)
					assert.Equal(t, TurnCompleted, event.Outcome)
					terminal = true
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("timed out waiting for %q turn settlement", content)
			}
		}
	}
}

func TestSessionRetryDoesNotAppendUserInputAndCorrelates(t *testing.T) {
	rt, sess := newSessionFixture(t)
	sess.AddMessage(session.UserMessage("original"))
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	before := len(sessionHandleSnapshot(t, h).Messages)
	obs, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer obs.Cancel()
	submission, err := h.Retry(t.Context())
	require.NoError(t, err)
	for envelope := range obs.Events {
		assert.Equal(t, submission.TurnID, envelope.TurnID)
		if _, ok := envelope.Event.(*StreamStoppedEvent); ok {
			break
		}
	}
	assert.Len(t, sessionHandleSnapshot(t, h).Messages, before+1, "retry appends only the assistant result")
}

func TestSessionObserverCancellationDoesNotStopSession(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	obs, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	obs.Cancel()
	select {
	case _, ok := <-obs.Events:
		assert.False(t, ok, "explicit observer cancellation must close its event tail")
	case <-time.After(time.Second):
		t.Fatal("explicit observer cancellation did not close its event tail")
	}
	_, err = h.Submit(t.Context(), TurnInput{Content: "still runs"})
	require.NoError(t, err)
}

func TestDeleteErrorOmitsMailboxCapacity(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))

	_, err = h.Submit(t.Context(), TurnInput{Content: "must stay stopped"})
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorStopped, sessionErr.Kind)
	assert.Zero(t, sessionErr.Limit)
	assert.Equal(t, "session submit: stopped", err.Error())
}

func TestDeleteTombstonesSessionAgainstStaleRecreation(t *testing.T) {
	rt, sess := newSessionFixture(t)
	h, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	require.NoError(t, rt.DeleteSession(t.Context(), sess.ID))

	_, err = h.Submit(t.Context(), TurnInput{Content: "stale handle"})
	require.ErrorIs(t, err, ErrSessionStopped)
	_, err = rt.CreateSession(t.Context(), sess.Clone(), SessionBinding{})
	var sessionErr *SessionError
	require.ErrorAs(t, err, &sessionErr)
	assert.Equal(t, SessionErrorStopped, sessionErr.Kind)
	assert.Equal(t, "session register: stopped", err.Error())
}
