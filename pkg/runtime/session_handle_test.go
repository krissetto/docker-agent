package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	assert.False(t, d.running(), "cleared retry must not start execution")
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
				assert.True(t, turnEnvelopeAdmissible(envelope, submission.TurnID, h.ID()), "envelope must belong to the turn or be a session-scoped SubagentTreeEvent")
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

// isTurnlessSessionTree admits a turn-less envelope only when it is exactly a
// journaled, session-scoped SubagentTreeEvent. Topology snapshots are journaled
// independently of runs (event.go), so one may be published between a turn's
// Accepted/Promoted and its StreamStarted and carry no TurnID. Gap, seed
// (sequence zero), foreign-session, and nil-payload envelopes are never admitted.
func isTurnlessSessionTree(envelope SessionEvent, sessionID string) bool {
	tree, ok := envelope.Event.(*SubagentTreeEvent)
	return ok && tree != nil && envelope.TurnID == "" && envelope.Sequence > 0 && !envelope.Gap && envelope.SessionID == sessionID
}

// turnEnvelopeAdmissible is the correlation rule for one observed envelope of a
// submitted turn: turn-owned envelopes must carry that turn's ID, and the only
// turn-less envelope is a session-scoped SubagentTreeEvent.
func turnEnvelopeAdmissible(envelope SessionEvent, turnID, sessionID string) bool {
	if envelope.TurnID == "" {
		return isTurnlessSessionTree(envelope, sessionID)
	}
	return envelope.TurnID == turnID
}

func TestObservationTurnlessClassification(t *testing.T) {
	const sessionID, turnID, otherTurn = "classification-session", "turn-1", "turn-2"
	tree := &SubagentTreeEvent{Type: "subagent_tree"}
	envelope := func(turn string, sequence uint64, event Event) SessionEvent {
		return SessionEvent{SessionID: sessionID, TurnID: turn, Sequence: sequence, Event: event}
	}
	// Interleaving: a turn-less tree snapshot between Accepted/Promoted and StreamStarted.
	interleaved := []SessionEvent{
		envelope(turnID, 1, PendingUserMessageAccepted(sessionID, turnID, "hello", nil, 0)),
		envelope("", 2, tree),
		envelope(turnID, 3, PendingUserMessagePromoted(sessionID, turnID, "hello", nil, 0)),
		envelope(turnID, 4, StreamStarted(sessionID, "root")),
	}
	for _, item := range interleaved {
		assert.True(t, turnEnvelopeAdmissible(item, turnID, sessionID), "interleaved envelope %T must be admitted", item.Event)
	}
	cases := []struct {
		name     string
		envelope SessionEvent
		want     bool
	}{
		{"turnless tree between turn events", envelope("", 2, tree), true},
		{"tree carrying the active turn ID", envelope(turnID, 2, tree), true},
		{"tree carrying a different turn ID", envelope(otherTurn, 2, tree), false},
		{"turnless started event", envelope("", 4, StreamStarted(sessionID, "root")), false},
		{"turnless agent choice", envelope("", 5, AgentChoice("root", sessionID, "text")), false},
		{"turnless tree at sequence zero", envelope("", 0, tree), false},
		{"turnless tree gap marker", SessionEvent{SessionID: sessionID, Sequence: 2, Gap: true, Event: tree}, false},
		{"turnless tree from another session", SessionEvent{SessionID: "other-session", Sequence: 2, Event: tree}, false},
		{"turnless nil tree payload", envelope("", 2, (*SubagentTreeEvent)(nil)), false},
		{"turn event for a different turn", envelope(otherTurn, 3, StreamStarted(sessionID, "root")), false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, turnEnvelopeAdmissible(tc.envelope, turnID, sessionID), tc.name)
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

func TestTailHistoryPagesAreBoundedAppendSafeAndMutationFenced(t *testing.T) {
	rt, sess := newSessionFixture(t)
	for i := 0; i < 5; i++ {
		sess.AddMessage(session.UserMessage(fmt.Sprintf("item-%d", i)))
	}
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	limits := SessionHistoryLimits{MaxItems: 2, MaxBytes: 2 << 20, MaxItemBytes: 128 << 10}
	obs, err := handle.Observe(t.Context(), ObserveOptions{HistoryTail: &limits})
	require.NoError(t, err)
	defer obs.Cancel()
	anchor := obs.Primary()
	require.Equal(t, "tail", anchor.Projection)
	require.Equal(t, 5, anchor.TranscriptPosition)
	require.Len(t, anchor.Items, 2)
	require.NotContains(t, anchor.Session.MessagesSnapshot(), session.Item{})
	cursor := anchor.HistoryCursor
	require.NotEmpty(t, cursor)
	// Appending after the snapshot does not slide the opaque history anchor.
	sess.AddMessage(session.UserMessage("newer"))
	page, err := handle.(SessionHistoryReader).ReadHistoryPage(t.Context(), sess.ID, cursor, limits)
	require.NoError(t, err)
	require.Equal(t, anchor.Epoch, page.Epoch)
	require.Equal(t, anchor.Cursor, page.Cursor)
	require.Equal(t, anchor.TranscriptPosition, page.TranscriptPosition)
	require.Equal(t, 5, page.History.TotalItems)
	require.Equal(t, []int{1, 2}, []int{page.Items[0].Position, page.Items[1].Position})
	// In-place edits invalidate the next cursor without invalidating append-only paging.
	driver := handle.(*sessionHandle).driver
	require.NoError(t, driver.ownerCall(t.Context(), func() error { driver.sess.MarkHistoryChanged(); return nil }))
	_, err = handle.(SessionHistoryReader).ReadHistoryPage(t.Context(), sess.ID, page.HistoryCursor, limits)
	require.ErrorIs(t, err, ErrHistoryCursorStale)
}

func TestTailHistoryRejectsWrongSessionCursor(t *testing.T) {
	rt, sess := newSessionFixture(t)
	sess.AddMessage(session.UserMessage("one"))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	limits := SessionHistoryLimits{MaxItems: 1, MaxBytes: 2 << 20, MaxItemBytes: 128 << 10}
	obs, err := handle.Observe(t.Context(), ObserveOptions{HistoryTail: &limits})
	require.NoError(t, err)
	defer obs.Cancel()
	_, err = handle.(SessionHistoryReader).ReadHistoryPage(t.Context(), "other", obs.Primary().HistoryCursor, limits)
	require.ErrorIs(t, err, ErrHistoryCursorInvalid)
}

func TestTailSnapshotDetachesPendingInputDocuments(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	payload := []byte("owned document bytes")
	parts := []chat.MessagePart{{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "file.txt", MimeType: "text/plain", Source: chat.DocumentSource{InlineData: payload}}}}
	driver := handle.(*sessionHandle).driver
	require.NoError(t, driver.ownerCall(t.Context(), func() error {
		driver.pending = []QueuedMessage{{ID: "pending-turn", Content: "queued", MultiContent: parts, AcceptedPosition: 0}}
		return nil
	}))
	limits := SessionHistoryLimits{MaxItems: 1, MaxBytes: 1 << 20, MaxItemBytes: 128 << 10}
	observation, err := handle.Observe(t.Context(), ObserveOptions{HistoryTail: &limits})
	require.NoError(t, err)
	defer observation.Cancel()
	pending := observation.Primary().PendingInputs
	require.Len(t, pending, 1)
	parts[0].Document.Source.InlineData[0] = 'X'
	require.Equal(t, []byte("owned document bytes"), pending[0].MultiContent[0].Document.Source.InlineData)
}

func TestTailSnapshotRejectsOversizedPendingBeforeDetachedCopy(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	payload := []byte("x")
	parts := []chat.MessagePart{{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "large", MimeType: "application/octet-stream", Source: chat.DocumentSource{InlineData: payload}}}}
	tooLarge := session.UserMessage(strings.Repeat("p", 2<<20))
	require.NoError(t, handle.(*sessionHandle).driver.ownerCall(t.Context(), func() error {
		handle.(*sessionHandle).driver.pending = []QueuedMessage{{ID: "large-pending", Content: tooLarge.Message.Content, MultiContent: parts}}
		return nil
	}))
	limits := SessionHistoryLimits{MaxItems: 1, MaxBytes: 16 << 10, MaxItemBytes: 4096}
	_, err = handle.Observe(t.Context(), ObserveOptions{HistoryTail: &limits})
	require.ErrorIs(t, err, ErrHistoryMetadataTooLarge)
}

func TestTailHistoryPacksCompleteRecordsAroundMetadataBudget(t *testing.T) {
	rt, sess := newSessionFixture(t)
	content := strings.Repeat("x", 132<<10)
	for i := 0; i < 16; i++ {
		sess.AddMessage(session.UserMessage(content))
	}
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	limits := SessionHistoryLimits{MaxItems: 20, MaxBytes: 2 << 20, MaxItemBytes: 256 << 10}
	observation, err := handle.Observe(t.Context(), ObserveOptions{HistoryTail: &limits})
	require.NoError(t, err)
	defer observation.Cancel()
	initial := observation.Primary()
	require.Len(t, initial.Items, 15, "full response envelope, not raw item bytes, determines fit")
	require.True(t, initial.History.HasMore)
	require.Empty(t, initial.History.OmittedResponsePositions, "the last complete record remains pageable when this page is full")
	require.NotEmpty(t, initial.HistoryCursor)
	page, err := handle.(SessionHistoryReader).ReadHistoryPage(t.Context(), sess.ID, initial.HistoryCursor, limits)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, 0, page.Items[0].Position)
	require.Equal(t, initial.Cursor, page.Cursor)
	require.Equal(t, initial.TranscriptPosition, page.TranscriptPosition)
}

func TestTailHistoryReportsItemThatCannotFitResponseBudget(t *testing.T) {
	rt, sess := newSessionFixture(t)
	sess.AddMessage(session.UserMessage(strings.Repeat("y", 20<<10)))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	limits := SessionHistoryLimits{MaxItems: 1, MaxBytes: 16 << 10, MaxItemBytes: 64 << 10}
	observation, err := handle.Observe(t.Context(), ObserveOptions{HistoryTail: &limits})
	require.NoError(t, err)
	defer observation.Cancel()
	tail := observation.Primary()
	require.Empty(t, tail.Items)
	require.Equal(t, []int{0}, tail.History.OmittedResponsePositions)
	require.False(t, tail.History.HasMore)
}

func TestTailHistoryReportsMixedOmissionRangesChronologically(t *testing.T) {
	rt, sess := newSessionFixture(t)
	for _, content := range []string{strings.Repeat("r", 15<<10), strings.Repeat("a", 2<<10), strings.Repeat("i", 17<<10), strings.Repeat("b", 2<<10), strings.Repeat("r", 15<<10)} {
		sess.AddMessage(session.UserMessage(content))
	}
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	limits := SessionHistoryLimits{MaxItems: 4, MaxBytes: 16 << 10, MaxItemBytes: 16 << 10}
	observation, err := handle.Observe(t.Context(), ObserveOptions{HistoryTail: &limits})
	require.NoError(t, err)
	defer observation.Cancel()
	page := observation.Primary()
	require.True(t, page.History.HasMore)
	ranges := historyPageWire(page).History.OmittedRanges
	require.Equal(t, []int{2, 4}, []int{ranges[0].StartPosition, ranges[1].StartPosition})
	require.Equal(t, "item_exceeds_item_byte_limit", ranges[0].Reason)
	require.Equal(t, "item_exceeds_response_byte_limit", ranges[1].Reason)
	older, err := handle.(SessionHistoryReader).ReadHistoryPage(t.Context(), sess.ID, page.HistoryCursor, limits)
	require.NoError(t, err)
	require.Equal(t, []int{0}, []int{older.Items[0].Position})
	olderRanges := historyPageWire(older).History.OmittedRanges
	require.Empty(t, olderRanges)
}

func TestTailHistoryUsesResponseByteOmissionWhenHeaderConsumesBudget(t *testing.T) {
	rt, sess := newSessionFixture(t)
	sess.AddMessage(session.UserMessage(strings.Repeat("y", 20<<10)))
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	limits := SessionHistoryLimits{MaxItems: 1, MaxBytes: 16 << 10, MaxItemBytes: 64 << 10}
	observation, err := handle.Observe(t.Context(), ObserveOptions{HistoryTail: &limits})
	require.NoError(t, err)
	defer observation.Cancel()
	page := observation.Primary()
	require.Empty(t, page.Items)
	require.Equal(t, []int{0}, page.History.OmittedResponsePositions)
	require.False(t, page.History.HasMore)
}
