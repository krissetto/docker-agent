// Package turn owns the lifetime of a synchronous host's accepted submission.
package turn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
)

const cleanupTimeout = 30 * time.Second

// Handle is the canonical submission and settlement boundary.
type Handle interface {
	Submit(ctx context.Context, input runtime.TurnInput) (runtime.Submission, error)
	Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error)
	Cancel(ctx context.Context, turnID string) (runtime.CancelResult, error)
	AwaitTurn(ctx context.Context, turnID string) error
}

// DrainError means the host must not reuse the session until its owner shuts it down.
type DrainError struct{ Err error }

func (e *DrainError) Error() string { return fmt.Sprintf("drain accepted turn: %v", e.Err) }
func (e *DrainError) Unwrap() error { return e.Err }

// Turn owns an observation and one accepted request, not the session or runtime.
// The caller must call Consume exactly once after Start succeeds.
type Turn struct {
	Submission  runtime.Submission
	handle      Handle
	observation runtime.Observation
}

// Start observes before admission. Observation detachment never cancels execution.
func Start(ctx context.Context, handle Handle, input runtime.TurnInput) (*Turn, error) {
	observation, err := runtimeclient.ObserveTurnStartup(ctx, handle)
	if err != nil {
		return nil, fmt.Errorf("observe session: %w", err)
	}
	submission, err := handle.Submit(ctx, input)
	if err != nil {
		if observation.Cancel != nil {
			observation.Cancel()
		}
		return nil, fmt.Errorf("submit turn: %w", err)
	}
	return &Turn{Submission: submission, handle: handle, observation: observation}, nil
}

// Consume retains admission ownership until the exact request settles, including
// when the caller cancels, a handler exits early, or observation fails.
func (t *Turn) Consume(ctx context.Context, handler func(context.Context, runtime.SessionEvent) (runtimeclient.TurnDecision, error)) runtimeclient.TurnTermination {
	var cancelOnce sync.Once
	var cancelErr error
	cancelTurn := func() {
		cancelOnce.Do(func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			_, cancelErr = t.handle.Cancel(cleanupCtx, t.Submission.TurnID)
		})
	}
	stopCancel := context.AfterFunc(ctx, cancelTurn)
	termination := runtimeclient.ConsumeAcceptedTurn(ctx, t.handle, t.observation, t.Submission.TurnID, handler)
	if !stopCancel() || !termination.Stopped {
		cancelTurn()
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := t.handle.AwaitTurn(cleanupCtx, t.Submission.TurnID); err != nil {
		termination.Err = errors.Join(termination.Err, &DrainError{Err: errors.Join(cancelErr, err)})
	} else {
		termination.Err = errors.Join(termination.Err, cancelErr)
	}
	return termination
}
