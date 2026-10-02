package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
)

// ErrTurnObservationEnded reports that an observation closed before the
// correlated StreamStopped event was received.
var ErrTurnObservationEnded = errors.New("turn observation ended before stream stopped")

// TurnDecision tells ConsumeTurn whether the host has finished handling a turn.
type TurnDecision uint8

const (
	TurnContinue TurnDecision = iota
	TurnTerminate
)

// TurnHandler handles correlated events for ConsumeTurnWithHandler.
type TurnHandler interface {
	HandleTurn(ctx context.Context, event runtime.SessionEvent) (TurnDecision, error)
}

// TurnHandlerFunc adapts a function to TurnHandler.
type TurnHandlerFunc func(context.Context, runtime.SessionEvent) (TurnDecision, error)

func (f TurnHandlerFunc) HandleTurn(ctx context.Context, envelope runtime.SessionEvent) (TurnDecision, error) {
	return f(ctx, envelope)
}

// TurnResult identifies the completed submission.
type TurnResult struct {
	Submission runtime.Submission
	StopReason string
}

// TurnEventError is the default error projection for a correlated ErrorEvent.
type TurnEventError struct {
	Code    string
	Message string
}

func (e *TurnEventError) Error() string { return e.Message }

// TurnTermination describes why ConsumeTurn returned. Err is set for context
// cancellation, an observation gap/failure, premature EOF, or a handler error.
type TurnTermination struct {
	Stopped          bool
	Terminated       bool
	ObservationError bool
	Err              error
}

// ObservationGapError reports that an observation can no longer provide a
// contiguous event history. Gap envelopes are checked before turn filtering.
type ObservationGapError struct {
	FirstAvailable uint64
}

func (e *ObservationGapError) Error() string {
	return fmt.Sprintf("observation gap: first available sequence %d", e.FirstAvailable)
}

// RunTurn runs one turn with default event handling. Runtime ErrorEvents are
// returned as *TurnEventError.
func RunTurn(ctx context.Context, session runtime.SessionHandle, input runtime.TurnInput) (TurnResult, error) {
	return runTurnFunc(ctx, session, input, nil, true)
}

// RunTurnFunc observes before submitting, invokes fn for ordinary correlated
// events, and returns after StreamStopped. Interactions receive safe default
// responses and are not delivered to fn.
func RunTurnFunc(ctx context.Context, session runtime.SessionHandle, input runtime.TurnInput, fn func(context.Context, runtime.SessionEvent) error) (TurnResult, error) {
	return runTurnFunc(ctx, session, input, fn, true)
}

// RunTurnFuncOwnedInteractions invokes fn for every correlated event and makes
// it responsible for responding to interactions.
func RunTurnFuncOwnedInteractions(ctx context.Context, session runtime.SessionHandle, input runtime.TurnInput, fn func(context.Context, runtime.SessionEvent) error) (TurnResult, error) {
	return runTurnFunc(ctx, session, input, fn, false)
}

func runTurnFunc(ctx context.Context, session runtime.SessionHandle, input runtime.TurnInput, fn func(context.Context, runtime.SessionEvent) error, defaultInteractions bool) (TurnResult, error) {
	var stopReason string
	handler := TurnHandlerFunc(func(ctx context.Context, envelope runtime.SessionEvent) (TurnDecision, error) {
		if event, ok := envelope.Event.(*runtime.ErrorEvent); ok {
			return TurnTerminate, &TurnEventError{Code: event.Code, Message: event.Error}
		}
		if stopped, ok := envelope.Event.(*runtime.StreamStoppedEvent); ok {
			stopReason = stopped.Reason
		}
		if defaultInteractions && isInteraction(envelope.Event) {
			if err := respondToInteraction(ctx, session, envelope); err != nil {
				return TurnTerminate, err
			}
			return TurnContinue, nil
		}
		if fn != nil {
			if err := fn(ctx, envelope); err != nil {
				return TurnTerminate, err
			}
		}
		return TurnContinue, nil
	})
	submission, termination, err := RunTurnWithHandler(ctx, session, input, handler)
	result := TurnResult{Submission: submission, StopReason: stopReason}
	if err != nil {
		return result, err
	}
	if termination.Err != nil {
		return result, termination.Err
	}
	if !termination.Stopped {
		return result, ErrTurnObservationEnded
	}
	return result, nil
}

func isInteraction(event runtime.Event) bool {
	switch event.(type) {
	case *runtime.ToolCallConfirmationEvent, *runtime.MaxIterationsReachedEvent, *runtime.ElicitationRequestEvent:
		return true
	default:
		return false
	}
}

func respondToInteraction(ctx context.Context, session runtime.SessionHandle, envelope runtime.SessionEvent) error {
	response := runtime.InteractionResponse{InteractionID: envelope.InteractionID}
	switch event := envelope.Event.(type) {
	case *runtime.ToolCallConfirmationEvent:
		response.Kind = runtime.InteractionConfirmation
		response.Resume = runtime.ResumeReject("No interactive approval handler was provided.")
	case *runtime.MaxIterationsReachedEvent:
		response.Kind = runtime.InteractionMaxIterations
		response.Resume = runtime.ResumeReject("")
	case *runtime.ElicitationRequestEvent:
		response.Kind = runtime.InteractionElicitation
		response.ElicitationID = event.ElicitationID
		response.Elicitation = runtime.ElicitationResult{Action: tools.ElicitationActionDecline}
	default:
		return nil
	}
	if err := session.Respond(ctx, response); err != nil {
		return fmt.Errorf("respond to %s interaction: %w", response.Kind, err)
	}
	return nil
}

// RunTurnWithHandler is the detailed runner for hosts that need TurnDecision
// and TurnTermination. It observes before submitting to avoid missing events.
func RunTurnWithHandler(ctx context.Context, session runtime.SessionHandle, input runtime.TurnInput, handler TurnHandler) (runtime.Submission, TurnTermination, error) {
	if session == nil || handler == nil {
		return runtime.Submission{}, TurnTermination{}, errors.New("session and handler are required")
	}
	observation, err := observeTurnStartup(ctx, session, observationRetryPolicy{wait: waitRetry, now: time.Now})
	if err != nil {
		return runtime.Submission{}, TurnTermination{}, fmt.Errorf("observe turn: %w", err)
	}
	if err := rejectTreeObservation(observation); err != nil {
		if observation.Cancel != nil {
			observation.Cancel()
		}
		return runtime.Submission{}, TurnTermination{}, err
	}
	submission, err := session.Submit(ctx, input)
	if err != nil {
		if observation.Cancel != nil {
			observation.Cancel()
		}
		return runtime.Submission{}, TurnTermination{}, fmt.Errorf("submit turn: %w", err)
	}
	termination := consumeAcceptedTurn(ctx, session, observation, submission.TurnID, handler.HandleTurn, observationRetryPolicy{wait: waitRetry, now: time.Now})
	return submission, termination, nil
}

// ConsumeTurn consumes an existing observation for one submitted turn.
//
// Replay is consumed before the live tail, gaps are detected before unrelated
// turns are filtered, and matching StreamStopped is delivered to the handler
// before termination. Context cancellation terminates consumption. The
// observation is canceled exactly once on return.
func ConsumeTurn(ctx context.Context, observation runtime.Observation, turnID string, handler func(context.Context, runtime.SessionEvent) (TurnDecision, error)) TurnTermination {
	return consumeTurn(ctx, observation, turnID, handler, nil, nil)
}

func consumeTurn(ctx context.Context, observation runtime.Observation, turnID string, handler func(context.Context, runtime.SessionEvent) (TurnDecision, error), cursor *uint64, progress func()) TurnTermination {
	if observation.Cancel != nil {
		defer observation.Cancel()
	}
	if handler == nil {
		return TurnTermination{Terminated: true, Err: errors.New("handler is required")}
	}
	if err := rejectTreeObservation(observation); err != nil {
		return TurnTermination{Terminated: true, ObservationError: true, Err: err}
	}

	consume := func(envelope runtime.SessionEvent) (bool, TurnTermination) {
		if envelope.Gap {
			return false, TurnTermination{ObservationError: true, Err: &ObservationGapError{FirstAvailable: envelope.FirstAvailable}}
		}
		if cursor != nil && envelope.Sequence != 0 {
			if envelope.Sequence <= *cursor {
				return true, TurnTermination{}
			}
			*cursor = envelope.Sequence
			if progress != nil {
				progress()
			}
		}
		if envelope.TurnID != turnID {
			return true, TurnTermination{}
		}

		_, stopped := envelope.Event.(*runtime.StreamStoppedEvent)
		decision, err := handler(ctx, envelope)
		if err != nil {
			return false, TurnTermination{Stopped: stopped, Terminated: true, Err: err}
		}
		if stopped {
			return false, TurnTermination{Stopped: true}
		}
		if decision == TurnTerminate {
			return false, TurnTermination{Terminated: true}
		}
		return true, TurnTermination{}
	}

	for _, envelope := range observation.Replay {
		if ctx.Err() != nil {
			return TurnTermination{Terminated: true, Err: ctx.Err()}
		}
		if more, termination := consume(envelope); !more {
			return termination
		}
	}
	events := observation.Events
	if events == nil {
		return TurnTermination{ObservationError: true, Err: ErrTurnObservationEnded}
	}
	errs := observation.Errors
	for {
		if ctx.Err() != nil {
			return TurnTermination{Terminated: true, Err: ctx.Err()}
		}
		select {
		case <-ctx.Done():
			return TurnTermination{Terminated: true, Err: ctx.Err()}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			if err != nil {
				// A remote reader can enqueue its final event and terminal error
				// together. Drain the bounded queued tail before reconnecting;
				// otherwise select could discard a completed turn or interaction.
				for {
					select {
					case envelope, open := <-events:
						if !open {
							return TurnTermination{ObservationError: true, Err: err}
						}
						if more, termination := consume(envelope); !more {
							return termination
						}
					default:
						return TurnTermination{ObservationError: true, Err: err}
					}
				}
			}
		case envelope, ok := <-events:
			if !ok {
				return observationEnded(errs)
			}
			if more, termination := consume(envelope); !more {
				return termination
			}
		}
	}
}

func observationEnded(errs <-chan error) TurnTermination {
	if errs != nil {
		select {
		case err, ok := <-errs:
			if ok && err != nil {
				return TurnTermination{ObservationError: true, Err: err}
			}
		default:
		}
	}
	return TurnTermination{ObservationError: true, Err: ErrTurnObservationEnded}
}

// observeTurnStartup retries only observation establishment, never submission.
func observeTurnStartup(ctx context.Context, session Observer, policy observationRetryPolicy) (runtime.Observation, error) {
	startup, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(30*time.Second, cancel)
	started := policy.now()
	attempt := 0
	for {
		observation, err := session.Observe(startup, runtime.ObserveOptions{})
		if err == nil {
			timer.Stop()
			original := observation.Cancel
			observation.Cancel = func() {
				cancel()
				if original != nil {
					original()
				}
			}
			return observation, nil
		}
		if startup.Err() != nil || !retryObservation(err) || policy.now().Sub(started) >= 30*time.Second || !policy.wait(startup, attempt) {
			timer.Stop()
			cancel()
			return runtime.Observation{}, err
		}
		attempt = min(attempt+1, 8)
	}
}

// Once accepted, only observation is retried. Neither reconnect nor context
// cancellation resubmits or cancels the server-owned turn. Gaps are explicit:
// a headless output handler cannot roll back already rendered deltas safely.
func consumeAcceptedTurn(ctx context.Context, session Observer, observation runtime.Observation, turnID string, handler func(context.Context, runtime.SessionEvent) (TurnDecision, error), policy observationRetryPolicy) TurnTermination {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	startup := time.AfterFunc(30*time.Second, cancel)
	defer startup.Stop()
	cursor := observation.Primary().Cursor
	attempt := 0
	for {
		before := cursor
		started := policy.now()
		reconnectable := observation.Errors != nil
		sustained := time.AfterFunc(5*time.Second, func() { startup.Stop() })
		termination := consumeTurn(ctx, observation, turnID, handler, &cursor, func() { startup.Stop() })
		sustained.Stop()
		if !termination.ObservationError || !reconnectable || ctx.Err() != nil {
			return termination
		}
		var gap *ObservationGapError
		if errors.As(termination.Err, &gap) || !retryObservation(termination.Err) {
			return termination
		}
		if cursor > before || policy.now().Sub(started) >= 5*time.Second {
			attempt = 0
		}
		for {
			if !policy.wait(ctx, attempt) {
				return TurnTermination{Terminated: true, Err: ctx.Err()}
			}
			attempt = min(attempt+1, 8)
			next, err := session.Observe(ctx, runtime.ObserveOptions{Since: &cursor})
			if err == nil {
				observation = next
				break
			}
			if ctx.Err() != nil {
				return TurnTermination{Terminated: true, Err: ctx.Err()}
			}
			if !retryObservation(err) {
				return TurnTermination{ObservationError: true, Err: err}
			}
		}
	}
}
