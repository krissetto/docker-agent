package embeddedchat

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
)

func borrowSession(t *testing.T, owner *Session, handler InteractionHandler) *Session {
	t.Helper()
	s, err := New(t.Context(), Config{SessionRuntime: owner.SessionRuntime(), SessionID: owner.handle.ID(), InteractionHandler: handler})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestBorrowedObserveOutstandingBaselineAndTail(t *testing.T) {
	owner, _ := newConfirmationSession(t, nil)
	owned, err := owner.Send(t.Context(), "use tool")
	require.NoError(t, err)
	original := confirmationEvent(t, owned)
	borrowed := borrowSession(t, owner, nil)
	out, err := borrowed.Observe(t.Context())
	require.NoError(t, err)
	baseline := receiveEvent(t, out)
	require.NotNil(t, baseline.Snapshot)
	require.Len(t, baseline.Snapshot.Interactions, 1)
	prompt := confirmationEvent(t, out)
	require.Equal(t, original.Interaction.InteractionID, prompt.Interaction.InteractionID)
	response := runtime.InteractionResponse{InteractionID: prompt.Interaction.InteractionID, Kind: prompt.Interaction.Kind, Resume: runtime.ResumeReject("safe decline")}
	require.NoError(t, borrowed.Respond(t.Context(), response))
	var resolved bool
	for !resolved {
		event := receiveEvent(t, out)
		_, resolved = event.RuntimeEvent.(*runtime.InteractionResolvedEvent)
	}
	require.Error(t, borrowed.Respond(t.Context(), response))
	for range owned {
	}
	require.NoError(t, owner.Restart())
	// The existing observer stays on its original conversation. A new wrapper
	// observes the fresh owner generation before submission to exercise live tail.
	live := borrowSession(t, owner, nil)
	liveOut, err := live.Observe(t.Context())
	require.NoError(t, err)
	require.NotNil(t, receiveEvent(t, liveOut).Snapshot)
	owned, err = owner.Send(t.Context(), "use tool again")
	require.NoError(t, err)
	prompt = confirmationEvent(t, liveOut)
	require.NotEqual(t, original.Interaction.InteractionID, prompt.Interaction.InteractionID)
	require.NoError(t, live.Respond(t.Context(), runtime.InteractionResponse{InteractionID: prompt.Interaction.InteractionID, Kind: prompt.Interaction.Kind, Resume: runtime.ResumeReject("safe decline")}))
	for range owned {
	}
}

func TestBorrowedObserveCloseLeavesOwnerAndOtherObserverAlive(t *testing.T) {
	owner, _ := newConfirmationSession(t, nil)
	owned, err := owner.Send(t.Context(), "use tool")
	require.NoError(t, err)
	original := confirmationEvent(t, owned)
	first, second := borrowSession(t, owner, nil), borrowSession(t, owner, nil)
	one, err := first.Observe(t.Context())
	require.NoError(t, err)
	two, err := second.Observe(t.Context())
	require.NoError(t, err)
	confirmationEvent(t, one)
	prompt := confirmationEvent(t, two)
	require.NoError(t, first.Close())
	for range one {
	}
	status, err := owner.handle.Status(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, status.TurnID)
	require.Equal(t, original.Interaction.InteractionID, prompt.Interaction.InteractionID)
	require.NoError(t, second.Respond(t.Context(), runtime.InteractionResponse{InteractionID: prompt.Interaction.InteractionID, Kind: prompt.Interaction.Kind, Resume: runtime.ResumeReject("safe decline")}))
	for range owned {
	}
	require.Error(t, first.Respond(t.Context(), runtime.InteractionResponse{InteractionID: prompt.Interaction.InteractionID}))
}

func TestBorrowedObserveHandlerDelayedAcrossRestart(t *testing.T) {
	owner, ts := newConfirmationSession(t, nil)
	owned, err := owner.Send(t.Context(), "use tool")
	require.NoError(t, err)
	original := confirmationEvent(t, owned)
	entered, cancelled, proceed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	defer release()
	borrowed := borrowSession(t, owner, func(ctx context.Context, _ runtime.InteractionSnapshot) (runtime.InteractionResponse, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-proceed
		return runtime.InteractionResponse{Resume: runtime.ResumeApprove()}, nil
	})
	out, err := borrowed.Observe(t.Context())
	require.NoError(t, err)
	<-entered
	before := owner.handle.ID()
	require.NoError(t, borrowed.Restart())
	<-cancelled
	require.NotEqual(t, before, borrowed.handle.ID())
	release()
	for range out {
	}
	require.Zero(t, ts.executions.Load())
	require.Error(t, borrowed.Respond(t.Context(), runtime.InteractionResponse{InteractionID: original.Interaction.InteractionID, Kind: original.Interaction.Kind, Resume: runtime.ResumeApprove()}))
	require.NoError(t, owner.Respond(t.Context(), runtime.InteractionResponse{InteractionID: original.Interaction.InteractionID, Kind: original.Interaction.Kind, Resume: runtime.ResumeReject("safe decline")}))
	for range owned {
	}
}

func TestObserveAndSendHandleInteractionOnce(t *testing.T) {
	var calls atomic.Int32
	s, ts := newConfirmationSession(t, func(context.Context, runtime.InteractionSnapshot) (runtime.InteractionResponse, error) {
		calls.Add(1)
		return runtime.InteractionResponse{Resume: runtime.ResumeReject("safe decline")}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	observed, err := s.Observe(ctx)
	require.NoError(t, err)
	require.NotNil(t, receiveEvent(t, observed).Snapshot)
	finished := make(chan struct{})
	go func() {
		for range observed {
		}
		close(finished)
	}()
	out, err := s.Send(t.Context(), "use tool")
	require.NoError(t, err)
	for event := range out {
		require.NoError(t, event.Err)
	}
	cancel()
	<-finished
	require.Equal(t, int32(1), calls.Load())
	require.Zero(t, ts.executions.Load())
}

type failedObservationHandle struct {
	*fakeRuntime
}

func (*failedObservationHandle) ID() string                        { return "failed" }
func (*failedObservationHandle) AgentName() string                 { return "root" }
func (*failedObservationHandle) Metadata() runtime.SessionMetadata { return runtime.SessionMetadata{} }

func (*failedObservationHandle) Observe(context.Context, runtime.ObserveOptions) (runtime.Observation, error) {
	return runtime.Observation{}, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, Operation: runtime.SessionOperationObserve}
}

func TestObserveTerminalFailureIsExplicit(t *testing.T) {
	s := &Session{handle: &failedObservationHandle{fakeRuntime: newFakeRuntime()}}
	out, err := s.Observe(t.Context())
	require.NoError(t, err)
	event := receiveEvent(t, out)
	var typed *runtime.SessionError
	require.ErrorAs(t, event.Err, &typed)
	select {
	case _, open := <-out:
		require.False(t, open)
	case <-time.After(time.Second):
		t.Fatal("failed observation did not detach")
	}
	require.NoError(t, s.Close())
}
