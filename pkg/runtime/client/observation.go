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

// ConnectionStateSink optionally receives observation connection transitions.
type ConnectionStateSink interface {
	OnConnectionState(connected bool, err error)
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
	a.runWithRetry(ctx, session, sink, observationRetryPolicy{wait: waitRetry, now: time.Now})
}

// The timing seam keeps outage/backoff tests deterministic without exposing
// transport tuning knobs to ordinary clients.
type observationRetryPolicy struct {
	wait func(context.Context, int) bool
	now  func() time.Time
}

func (a *Attachment) runWithRetry(ctx context.Context, session Observer, sink Sink, policy observationRetryPolicy) {
	defer close(a.done)
	originalCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	startup := time.AfterFunc(30*time.Second, cancel)
	defer startup.Stop()
	healthy := false
	initial := policy.now()
	gaps := 0
	var cursor *uint64
	var epoch string
	attempt := 0
	connection, reportsConnection := sink.(ConnectionStateSink)
	for ctx.Err() == nil {
		observation, err := session.Observe(ctx, runtime.ObserveOptions{Since: cursor, SinceEpoch: epoch})
		if err == nil {
			if reportsConnection && ctx.Err() == nil {
				connection.OnConnectionState(true, nil)
			}
			started := policy.now()
			sustained := time.AfterFunc(5*time.Second, func() { startup.Stop() })
			result := projectObservationWithEpoch(ctx, sink, observation, cursor, epoch, func() { startup.Stop() })
			sustained.Stop()
			if observation.Cancel != nil {
				observation.Cancel()
			}
			if ctx.Err() != nil {
				if originalCtx.Err() == nil {
					sink.OnError(errors.New("session observation startup timed out"))
				}
				return
			}
			if result.progress || policy.now().Sub(started) >= 5*time.Second {
				healthy = true
				startup.Stop()
			}
			if result.progress {
				gaps = 0
			}
			if result.gap {
				// A gap needs a fresh baseline, including outstanding interactions. Apply
				// backoff even here: a persistently overflowing observer must not spin.
				cursor = nil
				epoch = ""
				gaps++
				if gaps >= 4 {
					sink.OnError(&RepeatedObservationGapError{})
					return
				}
			} else {
				value := result.cursor
				cursor = &value
				epoch = result.epoch
				if result.err == nil {
					return
				}
				if result.progress || policy.now().Sub(started) >= 5*time.Second {
					attempt = 0
				}
			}
			err = result.err
		}
		if ctx.Err() != nil {
			if originalCtx.Err() == nil {
				sink.OnError(errors.New("session observation startup timed out"))
			}
			return
		}
		if !healthy && policy.now().Sub(initial) >= 30*time.Second {
			sink.OnError(errors.New("session observation startup timed out"))
			return
		}
		if err != nil && !retryObservation(err) {
			sink.OnError(fmt.Errorf("session observation failed: %w", err))
			return
		}
		if reportsConnection {
			connection.OnConnectionState(false, err)
		}
		if !policy.wait(ctx, attempt) {
			if ctx.Err() != nil && originalCtx.Err() == nil {
				sink.OnError(errors.New("session observation startup timed out"))
			}
			return
		}
		// Saturate before shifting; outages can outlast any fixed attempt budget.
		if attempt < 8 {
			attempt++
		}
	}
}

func retryObservation(err error) bool {
	var treeErr *TreeObservationError
	if errors.As(err, &treeErr) {
		return false
	}
	var classified interface{ Retryable() bool }
	if errors.As(err, &classified) {
		return classified.Retryable()
	}
	var sessionErr *runtime.SessionError
	if errors.As(err, &sessionErr) {
		return sessionErr.Kind == runtime.SessionErrorPersistence || sessionErr.Kind == runtime.SessionErrorCapacity
	}
	// Provider/network disconnects without a classification retain the historic
	// retry behavior, but now remain recoverable until the attachment is detached.
	return true
}

type projectionResult struct {
	cursor   uint64
	epoch    string
	gap      bool
	progress bool
	err      error
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
	if len(observation.Initial) > 1 || observation.SessionsAdded != nil || observation.TreeUpdates != nil {
		return &TreeObservationError{InitialSessions: len(observation.Initial), SessionsAdded: observation.SessionsAdded != nil || observation.TreeUpdates != nil}
	}
	return nil
}

// advanceObservation checks authority and gaps before deduplication or host filtering.
// A nil cursor retains the legacy ConsumeTurn contract without sequence filtering.
func advanceObservation(envelope runtime.SessionEvent, epoch string, cursor *uint64) (bool, error) {
	if envelope.Gap {
		return false, &ObservationGapError{FirstAvailable: envelope.FirstAvailable}
	}
	if envelope.Epoch != epoch {
		var sequence uint64
		if cursor != nil {
			sequence = *cursor
		}
		return false, &ObservationDiscontinuityError{PreviousEpoch: epoch, CurrentEpoch: envelope.Epoch, Cursor: sequence}
	}
	if cursor != nil && envelope.Sequence != 0 {
		if envelope.Sequence <= *cursor {
			return false, nil
		}
		*cursor = envelope.Sequence
	}
	return true, nil
}

func waitRetry(ctx context.Context, attempt int) bool {
	delay := min(time.Duration(1<<min(max(attempt, 0), 8))*25*time.Millisecond, 5*time.Second)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// RepeatedObservationGapError stops an observer which cannot establish a
// usable baseline. Retrying forever would hide a protocol or buffer mismatch.
type RepeatedObservationGapError struct{}

func (*RepeatedObservationGapError) Error() string {
	return "session observation repeated gaps without progress"
}
func (*RepeatedObservationGapError) Retryable() bool { return false }

func projectObservation(ctx context.Context, sink Sink, observation runtime.Observation, since *uint64) projectionResult {
	return projectObservationWithProgress(ctx, sink, observation, since, func() {})
}

func projectObservationWithProgress(ctx context.Context, sink Sink, observation runtime.Observation, since *uint64, onProgress func()) projectionResult {
	return projectObservationWithEpoch(ctx, sink, observation, since, "", onProgress)
}

func projectObservationWithEpoch(ctx context.Context, sink Sink, observation runtime.Observation, since *uint64, sinceEpoch string, onProgress func()) projectionResult {
	if ctx.Err() != nil {
		return projectionResult{}
	}
	if err := rejectTreeObservation(observation); err != nil {
		return projectionResult{err: err}
	}
	primary := observation.Primary()
	cursor := primary.Cursor
	progress := false
	if since == nil || primary.Epoch != sinceEpoch {
		sink.Reset(primary)
	} else {
		// Reconnect replay extends the existing projection; the newer snapshot
		// includes commits but omits the uncommitted streaming tail.
		cursor = *since
	}
	apply := func(envelope runtime.SessionEvent) bool {
		apply, err := advanceObservation(envelope, primary.Epoch, &cursor)
		if err != nil {
			return false
		}
		if !apply {
			return true
		}
		sink.Apply(envelope)
		progress = true
		onProgress()
		return true
	}
	for _, envelope := range observation.Replay {
		if !apply(envelope) {
			return projectionResult{cursor: cursor, epoch: primary.Epoch, progress: progress, gap: true}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return projectionResult{cursor: cursor, epoch: primary.Epoch, progress: progress}
		case envelope, ok := <-observation.Events:
			if !ok {
				if observation.Errors != nil {
					select {
					case err, open := <-observation.Errors:
						if open && err != nil {
							return projectionResult{cursor: cursor, epoch: primary.Epoch, progress: progress, err: err}
						}
					case <-ctx.Done():
						return projectionResult{cursor: cursor, epoch: primary.Epoch, progress: progress}
					}
				}
				return projectionResult{cursor: cursor, epoch: primary.Epoch, progress: progress, err: errors.New("observation stream closed")}
			}
			if !apply(envelope) {
				return projectionResult{cursor: cursor, epoch: primary.Epoch, progress: progress, gap: true}
			}
		}
	}
}
