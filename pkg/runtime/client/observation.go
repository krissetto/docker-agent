// Package client provides ordinary-client adapters over exported session contracts.
package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
)

type Observer interface {
	Observe(ctx context.Context, options runtime.ObserveOptions) (runtime.Observation, error)
}

type Sink interface {
	Reset(snapshot runtime.SessionSnapshot)
	Apply(envelope runtime.SessionEvent)
	OnError(err error)
}

type Attachment struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func Attach(ctx context.Context, session Observer, sink Sink) (*Attachment, error) {
	if session == nil || sink == nil {
		return nil, errors.New("session and sink are required")
	}
	child, cancel := context.WithCancel(ctx) // #nosec G118 -- Attachment exposes synchronous Detach.
	a := &Attachment{cancel: cancel, done: make(chan struct{})}
	go a.run(child, session, sink)
	return a, nil
}

func (a *Attachment) Detach() {
	if a == nil {
		return
	}
	a.once.Do(a.cancel)
	<-a.done
}

func (a *Attachment) run(ctx context.Context, session Observer, sink Sink) {
	defer close(a.done)
	var cursor *uint64
	const attempts = 4
	for attempt := 0; attempt < attempts && ctx.Err() == nil; attempt++ {
		observation, err := session.Observe(ctx, runtime.ObserveOptions{Since: cursor})
		if err != nil {
			if attempt == attempts-1 || !waitRetry(ctx, attempt) {
				if ctx.Err() == nil {
					sink.OnError(fmt.Errorf("attach failed after %d attempts: %w", attempt+1, err))
				}
				return
			}
			continue
		}
		result := projectObservation(ctx, sink, observation)
		observation.Cancel()
		var treeErr *TreeObservationError
		if errors.As(result.err, &treeErr) {
			sink.OnError(result.err)
			return
		}
		if result.gap {
			if attempt == attempts-1 {
				sink.OnError(errors.New("session observation gap after retry exhaustion"))
				return
			}
			cursor = nil
			continue
		}
		if result.cursor != 0 {
			value := result.cursor
			cursor = &value
		}
		if result.err == nil || ctx.Err() != nil {
			return
		}
		if attempt == attempts-1 || !waitRetry(ctx, attempt) {
			sink.OnError(fmt.Errorf("observation failed after %d attempts at cursor %d: %w", attempt+1, result.cursor, result.err))
			return
		}
	}
}

type projectionResult struct {
	cursor uint64
	gap    bool
	err    error
}

// TreeObservationError reports that an ordinary-client helper was given a
// tree observation whose descendant sessions cannot be represented by its
// single-session projection.
type TreeObservationError struct {
	InitialSessions int
	SessionsAdded   bool
}

func (e *TreeObservationError) Error() string {
	return "tree observations are unsupported by single-session client helpers"
}

func rejectTreeObservation(observation runtime.Observation) error {
	if len(observation.Initial) > 1 || observation.SessionsAdded != nil {
		return &TreeObservationError{InitialSessions: len(observation.Initial), SessionsAdded: observation.SessionsAdded != nil}
	}
	return nil
}

func waitRetry(ctx context.Context, attempt int) bool {
	timer := time.NewTimer(time.Duration(1<<attempt) * 25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func projectObservation(ctx context.Context, sink Sink, observation runtime.Observation) projectionResult {
	if ctx.Err() != nil {
		return projectionResult{}
	}
	if err := rejectTreeObservation(observation); err != nil {
		return projectionResult{err: err}
	}
	primary := observation.Primary()
	sink.Reset(primary)
	cursor := primary.Cursor
	apply := func(envelope runtime.SessionEvent) bool {
		if envelope.Gap {
			return false
		}
		if envelope.Sequence != 0 && envelope.Sequence <= cursor {
			return true
		}
		if envelope.Sequence != 0 {
			cursor = envelope.Sequence
		}
		sink.Apply(envelope)
		return true
	}
	for _, envelope := range observation.Replay {
		if !apply(envelope) {
			return projectionResult{cursor: cursor, gap: true}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return projectionResult{cursor: cursor}
		case envelope, ok := <-observation.Events:
			if !ok {
				if observation.Errors != nil {
					if err, open := <-observation.Errors; open && err != nil {
						return projectionResult{cursor: cursor, err: err}
					}
				}
				return projectionResult{cursor: cursor, err: errors.New("observation stream closed")}
			}
			if !apply(envelope) {
				return projectionResult{cursor: cursor, gap: true}
			}
		}
	}
}
