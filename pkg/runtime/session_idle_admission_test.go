package runtime

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdleSteerAcceptedEventFollowsDurablePromotion(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver
	// CreateSession's maintenance pass can wake the parked steer first; wait for
	// it to drain so this Steer observes its own idle admission.
	require.Eventually(t, func() bool { return driverDrained(driver) }, time.Second, time.Millisecond)
	observation, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 32})
	require.NoError(t, err)
	defer observation.Cancel()

	submission, err := handle.Steer(t.Context(), TurnInput{Content: "immediate guide", RequestID: "immediate"})
	require.NoError(t, err)
	require.Empty(t, submission.Disposition, "the idle FIFO head was promoted during the start attempt")

	var acceptedSequence, promotedSequence uint64
	require.Eventually(t, func() bool {
		for {
			select {
			case event := <-observation.Events:
				switch input := event.Event.(type) {
				case *PendingUserMessageAcceptedEvent:
					if input.TurnID == submission.TurnID {
						assert.True(t, input.Immediate)
						acceptedSequence = event.Sequence
					}
				case *PendingUserMessagePromotedEvent:
					if input.TurnID == submission.TurnID {
						promotedSequence = event.Sequence
					}
				}
			default:
				return acceptedSequence != 0 && promotedSequence != 0
			}
		}
	}, time.Second, time.Millisecond)
	assert.Less(t, acceptedSequence, promotedSequence, "Accepted remains ordered before Promoted")
	assert.Eventually(t, func() bool {
		status, statusErr := handle.Status(t.Context())
		return statusErr == nil && status.Pending == 0
	}, time.Second, time.Millisecond)
}

func TestIdleSteerRejectedStartPublishesOrdinaryPendingAdmission(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver
	driver.SetPreStartErrorGate(func() error { return errors.New("start blocked") }, nil)
	observation, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 32})
	require.NoError(t, err)
	defer observation.Cancel()

	submission, err := handle.Steer(t.Context(), TurnInput{Content: "still queued", RequestID: "blocked"})
	require.NoError(t, err)
	assert.Equal(t, SubmissionDispositionQueued, submission.Disposition)
	var accepted *PendingUserMessageAcceptedEvent
	require.Eventually(t, func() bool {
		select {
		case event := <-observation.Events:
			accepted, _ = event.Event.(*PendingUserMessageAcceptedEvent)
			return accepted != nil
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	assert.False(t, accepted.Immediate)
	status, err := handle.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, status.Pending)
	snapshot := sessionHandleSnapshot(t, handle)
	require.Len(t, pendingTurnIDs(snapshot), 1)
}

func TestConcurrentIdleIdempotentRetryDoesNotDowngradeImmediateAdmission(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver
	observation, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 64})
	require.NoError(t, err)
	defer observation.Cancel()

	entered, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseGate)
	driver.beforePrepareStart = func() {
		if starts.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	// The registry scheduler wakes a resident session with pending input, so it can
	// start the parked FIFO head before the duplicate is observed; hold all starts.
	parkStarts := func() { <-release }
	driver.beforeAnyPrepareStart.Store(&parkStarts)
	input := TurnInput{Content: "same idle input", RequestID: "same"}
	first := make(chan Submission, 1)
	firstErr := make(chan error, 1)
	go func() {
		submission, submitErr := handle.Submit(t.Context(), input)
		first <- submission
		firstErr <- submitErr
	}()
	waitClosed(t, entered, "idle submission reached start boundary")
	requestID, err := sessionInputID(handle.ID(), input.RequestID)
	require.NoError(t, err)
	duplicate, duplicateErr := handle.Submit(t.Context(), input)
	require.NoError(t, duplicateErr, "exact duplicate admission is idempotent while the original start is pending")
	assert.Equal(t, requestID, duplicate.TurnID)
	assert.Equal(t, SubmissionDispositionQueued, duplicate.Disposition, "duplicate reports retained mailbox state until original start resolves")
	releaseGate()
	original := <-first
	require.NoError(t, <-firstErr)
	assert.Equal(t, duplicate.TurnID, original.TurnID)

	var acceptedCount, immediateCount, promotedCount atomic.Int32
	require.Eventually(t, func() bool {
		for {
			select {
			case event := <-observation.Events:
				switch input := event.Event.(type) {
				case *PendingUserMessageAcceptedEvent:
					if input.TurnID == original.TurnID {
						acceptedCount.Add(1)
						if input.Immediate {
							immediateCount.Add(1)
						}
					}
				case *PendingUserMessagePromotedEvent:
					if input.TurnID == original.TurnID {
						promotedCount.Add(1)
					}
				}
			default:
				return promotedCount.Load() == 1
			}
		}
	}, time.Second, time.Millisecond)
	assert.EqualValues(t, 1, acceptedCount.Load())
	assert.EqualValues(t, 1, immediateCount.Load(), "duplicate retry cannot publish a conservative downgrade")
	assert.EqualValues(t, 1, promotedCount.Load())
}

func TestIdleDeferredAcceptedFlushesBeforePendingControlTransitions(t *testing.T) {
	for _, action := range []string{"cancel", "edit", "stop"} {
		t.Run(action, func(t *testing.T) {
			rt, sess := newSessionFixture(t)
			handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
			require.NoError(t, err)
			driver := handle.(*sessionHandle).driver
			observation, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 32})
			require.NoError(t, err)
			defer observation.Cancel()

			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseGate)
			hook := func() {
				enteredOnce.Do(func() { close(entered) })
				<-release
			}
			driver.beforeAnyPrepareStart.Store(&hook)
			input := TurnInput{Content: "controlled idle input", RequestID: "controlled"}
			requestID, err := sessionInputID(handle.ID(), input.RequestID)
			require.NoError(t, err)
			type submitResult struct {
				submission Submission
				err        error
			}
			submitted := make(chan submitResult, 1)
			go func() {
				submission, submitErr := handle.Submit(t.Context(), input)
				submitted <- submitResult{submission: submission, err: submitErr}
			}()
			waitClosed(t, entered, "idle input deferred before start")

			var transitionType string
			switch action {
			case "cancel":
				withdrawn, cancelErr := handle.(PendingMessageCanceler).CancelPendingMessage(t.Context(), requestID)
				require.NoError(t, cancelErr)
				require.True(t, withdrawn)
				transitionType = "pending_user_message_canceled"
			case "edit":
				_, editErr := handle.Edit(t.Context(), SessionEdit{Kind: SessionEditPendingMessage, PendingMessage: &PendingMessageEdit{TurnID: requestID, Content: "edited while start waits"}})
				require.NoError(t, editErr)
				transitionType = "pending_user_message_edited"
			case "stop":
				driver.StopAll()
			}
			releaseGate()
			result := <-submitted
			// Stop may fence the otherwise accepted submit while cancel/edit leave a
			// normal accepted result; either way the ordered admission evidence must
			// be flushed and removed from the deferred publisher.
			if action != "stop" {
				require.NoError(t, result.err)
			} else if result.err != nil {
				assert.ErrorIs(t, result.err, ErrSessionStopped)
			}

			acceptedSequence := uint64(0)
			transitionSequence := uint64(0)
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			for acceptedSequence == 0 || (transitionType != "" && transitionSequence == 0) {
				select {
				case event := <-observation.Events:
					switch value := event.Event.(type) {
					case *PendingUserMessageAcceptedEvent:
						if value.TurnID == requestID {
							assert.False(t, value.Immediate, "control transition keeps admission truthfully pending")
							acceptedSequence = event.Sequence
						}
					case *PendingUserMessageCanceledEvent:
						if value.TurnID == requestID {
							transitionSequence = event.Sequence
						}
					case *PendingUserMessageEditedEvent:
						if value.TurnID == requestID {
							transitionSequence = event.Sequence
						}
					}
				case <-deadline.C:
					t.Fatalf("timed out waiting for accepted/control events: accepted=%d transition=%d", acceptedSequence, transitionSequence)
				}
			}
			if transitionSequence != 0 {
				assert.Less(t, acceptedSequence, transitionSequence, "Accepted must precede cancel/edit")
			}
			var deferred int
			require.NoError(t, driver.ownerCall(t.Context(), func() error {
				deferred = len(driver.deferredAccepted)
				return nil
			}))
			assert.Zero(t, deferred, "control completion cannot strand deferred acceptance")
		})
	}
}

// Ungated scheduler-first interleaving: the registry promotes the parked head before
// the duplicate arrives. Duplicate must report no queued disposition and the same
// TurnID; events show one immediate Accepted ordered before one Promoted.
func TestSchedulerFirstPromotionKeepsDuplicateTruthful(t *testing.T) {
	rt, sess := newSessionFixture(t)
	handle, err := rt.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	driver := handle.(*sessionHandle).driver
	observation, err := handle.Observe(t.Context(), ObserveOptions{Buffer: 64})
	require.NoError(t, err)
	defer observation.Cancel()

	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseGate)
	var starts atomic.Int32
	driver.beforePrepareStart = func() {
		if starts.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	input := TurnInput{Content: "same idle input", RequestID: "same"}
	requestID, err := sessionInputID(handle.ID(), input.RequestID)
	require.NoError(t, err)
	first := make(chan Submission, 1)
	firstErr := make(chan error, 1)
	go func() {
		submission, submitErr := handle.Submit(t.Context(), input)
		first <- submission
		firstErr <- submitErr
	}()
	waitClosed(t, entered, "idle submission parked before start")

	var acceptedCount, immediateCount, promotedCount int
	var acceptedSeq, promotedSeq uint64
	record := func(event SessionEvent) {
		switch value := event.Event.(type) {
		case *PendingUserMessageAcceptedEvent:
			if value.TurnID == requestID {
				acceptedCount++
				acceptedSeq = event.Sequence
				if value.Immediate {
					immediateCount++
				}
			}
		case *PendingUserMessagePromotedEvent:
			if value.TurnID == requestID {
				promotedCount++
				promotedSeq = event.Sequence
			}
		}
	}
	rt.sessionDrivers.signalWork()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for promotedCount == 0 {
		select {
		case event := <-observation.Events:
			record(event)
		case <-deadline.C:
			t.Fatal("registry scheduler must promote the parked input before the duplicate")
		}
	}

	duplicate, duplicateErr := handle.Submit(t.Context(), input)
	require.NoError(t, duplicateErr)
	assert.Equal(t, requestID, duplicate.TurnID)
	assert.Empty(t, duplicate.Disposition, "input already promoted; duplicate must not report queued")
	for drained := false; !drained; {
		select {
		case event := <-observation.Events:
			record(event)
		default:
			drained = true
		}
	}
	assert.Equal(t, 1, acceptedCount)
	assert.Equal(t, 1, immediateCount, "scheduler promotion publishes Accepted as immediate; duplicate cannot downgrade")
	assert.Equal(t, 1, promotedCount)
	assert.Less(t, acceptedSeq, promotedSeq, "Accepted precedes Promoted")

	releaseGate()
	original := <-first
	require.NoError(t, <-firstErr)
	assert.Equal(t, duplicate.TurnID, original.TurnID)
}
