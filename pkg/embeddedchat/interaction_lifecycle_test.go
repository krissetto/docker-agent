package embeddedchat

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

func newBlockingSession(t *testing.T) (*Session, *blockingToolSet, func()) {
	t.Helper()
	tool := &blockingToolSet{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(tool.release) })
	tm := team.New(team.WithAgents(agent.New("root", "Use the tool.", agent.WithModel(&blockingToolProvider{}), agent.WithToolSets(tool))))
	s, err := New(t.Context(), Config{Team: tm, SessionOptions: []session.Opt{session.WithToolsApproved(true)}})
	require.NoError(t, err)
	t.Cleanup(func() {
		release()
		require.NoError(t, s.Close())
		require.NoError(t, tm.StopToolSets(context.WithoutCancel(t.Context())))
	})
	return s, tool, release
}

func TestRestartActiveCanonicalSessionWaitsForCleanup(t *testing.T) {
	for _, closeAfter := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%v", closeAfter), func(t *testing.T) {
			s, tool, release := newBlockingSession(t)
			out, err := s.Send(t.Context(), "run blocking fake tool")
			require.NoError(t, err)
			<-tool.started
			before := s.handle.ID()
			result := make(chan error, 1)
			go func() { result <- s.Restart() }()
			<-tool.cancelled
			select {
			case err := <-result:
				t.Fatalf("Restart returned before tool cleanup: %v", err)
			default:
			}
			_, err = s.Send(t.Context(), "cannot race generation")
			require.ErrorIs(t, err, ErrRunActive)
			closed := make(chan error, 1)
			if closeAfter {
				go func() { closed <- s.Close() }()
			}
			release()
			require.NoError(t, <-result)
			for range out {
			}
			require.NotEqual(t, before, s.handle.ID())
			if closeAfter {
				require.NoError(t, <-closed)
				_, err = s.Send(t.Context(), "closed")
				require.ErrorIs(t, err, ErrClosed)
			} else {
				for range 3 {
					require.NoError(t, s.Restart())
				}
				next, err := s.Send(t.Context(), "next")
				require.NoError(t, err)
				var done bool
				for event := range next {
					require.NoError(t, event.Err)
					done = done || event.Done
				}
				require.True(t, done)
			}
		})
	}
}

func TestRestartTimeoutRetainsCanonicalAuthority(t *testing.T) {
	s, tool, release := newBlockingSession(t)
	out, err := s.Send(t.Context(), "run blocking fake tool")
	require.NoError(t, err)
	<-tool.started
	before := s.handle.ID()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.restart(ctx), context.DeadlineExceeded)
	<-tool.cancelled
	require.Equal(t, before, s.handle.ID())
	_, err = s.Send(t.Context(), "quarantined")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = s.handle.Status(t.Context())
	require.NoError(t, err, "failed restart must not release the owned handle")
	release()
	for range out {
	}
	require.NoError(t, s.Restart())
	require.NotEqual(t, before, s.handle.ID())
}

func newConfirmationSession(t *testing.T, handler InteractionHandler) (*Session, *confirmationTools) {
	t.Helper()
	ts := &confirmationTools{}
	tm := team.New(team.WithAgents(agent.New("root", "Use probe.", agent.WithModel(confirmationProvider{}), agent.WithToolSets(ts))))
	s, err := New(t.Context(), Config{Team: tm, InteractionHandler: handler})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s, ts
}

func TestInteractionHandlerReceivesCanonicalConfirmation(t *testing.T) {
	for _, answer := range []string{"approve", "deny", "error", "empty"} {
		t.Run(answer, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var handled atomic.Int32
			callbackErr := errors.New("callback failed")
			s, ts := newConfirmationSession(t, func(_ context.Context, interaction runtime.InteractionSnapshot) (runtime.InteractionResponse, error) {
				handled.Add(1)
				require.Equal(t, runtime.InteractionConfirmation, interaction.Kind)
				require.NotEmpty(t, interaction.SessionID)
				require.NotEmpty(t, interaction.InteractionID)
				response := runtime.InteractionResponse{Resume: runtime.ResumeReject("denied")}
				if answer == "approve" {
					response.Resume = runtime.ResumeApprove()
				}
				if answer == "error" {
					return response, callbackErr
				}
				if answer == "empty" {
					return runtime.InteractionResponse{}, nil
				}
				return response, nil
			})
			out, err := s.Send(ctx, "use tool")
			require.NoError(t, err)
			var sawError, done bool
			for event := range out {
				if event.Err != nil {
					if answer == "error" {
						require.ErrorIs(t, event.Err, callbackErr)
					} else {
						require.Equal(t, "empty", answer)
					}
					sawError = true
				}
				done = done || event.Done
			}
			require.Equal(t, int32(1), handled.Load())
			assert.Equal(t, answer == "error" || answer == "empty", sawError)
			assert.Equal(t, answer == "approve" || answer == "deny", done)
			want := int32(0)
			if answer == "approve" {
				want = 1
			}
			assert.Equal(t, want, ts.executions.Load(), "only explicit approval can execute")
		})
	}
}

func confirmationEvent(t *testing.T, out <-chan Event) Event {
	t.Helper()
	for {
		select {
		case event, ok := <-out:
			require.True(t, ok)
			require.NoError(t, event.Err)
			if event.Interaction != nil {
				require.NotNil(t, event.Tool)
				require.True(t, event.Tool.NeedsConfirmation)
				require.Equal(t, event.Interaction.InteractionID, event.Tool.RequestID)
				return event
			}
		case <-time.After(5 * time.Second):
			t.Fatal("confirmation not received")
		}
	}
}

func TestConfirmationRespondAndConfirmSingleWinnerAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s, ts := newConfirmationSession(t, nil)
	out, err := s.Send(ctx, "use tool")
	require.NoError(t, err)
	old := confirmationEvent(t, out).Interaction
	require.Zero(t, ts.executions.Load())
	require.NoError(t, s.Restart())
	for range out {
	}
	out, err = s.Send(ctx, "use tool again")
	require.NoError(t, err)
	current := confirmationEvent(t, out).Interaction
	require.NotEqual(t, old.InteractionID, current.InteractionID)
	stale := runtime.InteractionResponse{InteractionID: old.InteractionID, Kind: old.Kind, Resume: runtime.ResumeApprove()}
	require.Error(t, s.Respond(ctx, stale))
	stale.Resume.RequestID = old.InteractionID
	require.Error(t, s.Confirm(ctx, stale.Resume))
	require.Zero(t, ts.executions.Load())
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		results <- s.Respond(ctx, runtime.InteractionResponse{InteractionID: current.InteractionID, Kind: current.Kind, Resume: runtime.ResumeApprove()})
	})
	wg.Go(func() {
		req := runtime.ResumeApprove()
		req.RequestID = current.InteractionID
		results <- s.Confirm(ctx, req)
	})
	wg.Wait()
	wins := 0
	for range 2 {
		if <-results == nil {
			wins++
		}
	}
	require.Equal(t, 1, wins)
	for event := range out {
		require.NoError(t, event.Err)
	}
	require.Equal(t, int32(1), ts.executions.Load())
}

func TestConfirmationHandlerReplyAfterRestartCannotApproveOldTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	entered, cancelled, proceed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	defer release()
	s, ts := newConfirmationSession(t, func(ctx context.Context, _ runtime.InteractionSnapshot) (runtime.InteractionResponse, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-proceed
		return runtime.InteractionResponse{Resume: runtime.ResumeApprove()}, nil
	})
	out, err := s.Send(ctx, "use tool")
	require.NoError(t, err)
	<-entered
	before := s.handle.ID()
	result := make(chan error, 1)
	go func() { result <- s.Restart() }()
	<-cancelled
	require.Zero(t, ts.executions.Load())
	release()
	require.NoError(t, <-result)
	for range out {
	}
	require.NotEqual(t, before, s.handle.ID())
	require.Zero(t, ts.executions.Load())
}
