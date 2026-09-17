package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

// EventObserver receives the runtime's event stream as it's produced.
// Implementations subscribe to lifecycle moments and act on them —
// persisting to a store, forwarding to a metrics pipeline, writing an
// audit transcript, etc.
//
// Concurrency: the runtime invokes observers synchronously from the
// goroutine that forwards events to the consumer's channel, in
// registration order. A slow observer therefore back-pressures both
// downstream observers and the consumer; long-running work (network
// I/O, file syncing) should fan out to a private goroutine.
//
// Errors: observers do not return errors. The runtime cannot recover
// from a misbehaving observer (it can't unregister it mid-stream and
// can't ask the consumer to retry), so an observer must log internally
// and never panic. The contract is "best-effort observation" rather
// than "all-or-nothing transactional".
//
// Observers see every event the runtime emits, including sub-session
// events (from delegated tasks via transfer_task) and
// [SessionScoped]-mismatch events. Filtering is the observer's
// responsibility; see [PersistenceObserver] for the canonical pattern.
type EventObserver interface {
	// OnRunStart fires once when [LocalRuntime.RunStream] begins, before
	// any event is dispatched. Use it for one-shot lifecycle work like
	// persisting initial session metadata.
	OnRunStart(ctx context.Context, sess *session.Session)
	// OnEvent fires once per event, after the runtime emits it but
	// before the consumer's channel receives it. Observers cannot
	// modify or suppress events (a future extension may relax this);
	// to drop an event from persistence, simply ignore it inside
	// OnEvent.
	OnEvent(ctx context.Context, sess *session.Session, event Event)
}

// WithEventObserver appends o to the runtime's observer chain.
// Observers are invoked in registration order, synchronously, on every
// event the runtime produces. Multiple calls are additive.
//
// The runtime auto-registers a [PersistenceObserver] for the configured
// session store; users do not need to wire persistence themselves.
// Custom observers (telemetry, audit, metrics, A2A forward) compose
// alongside that one.
func WithEventObserver(o EventObserver) Opt {
	return func(r *LocalRuntime) {
		if o == nil {
			return
		}
		r.observers = append(r.observers, o)
	}
}

func (r *LocalRuntime) observeRunStart(ctx context.Context, sess *session.Session) {
	for _, obs := range r.observers {
		if persistence, ok := obs.(*PersistenceObserver); ok {
			attemptCtx, cancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
			obs.OnRunStart(attemptCtx, sess)
			err := persistence.pendingError(sess.ID)
			attemptExpired := errors.Is(err, context.DeadlineExceeded) && attemptCtx.Err() != nil
			cancel()
			delay := subagentPersistenceRetryBase
			retry := func() (error, bool) {
				retryCtx, cancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
				defer cancel()
				err := persistence.flushContext(retryCtx, sess.ID)
				return err, errors.Is(err, context.DeadlineExceeded) && retryCtx.Err() != nil
			}
			for err != nil {
				// A local attempt deadline is retryable only while the owning run
				// is still alive. Never launch the provider before this FIFO drains.
				if attemptExpired && ctx.Err() == nil {
					err = &session.TemporaryError{Err: err}
				}
				if d, found := r.sessionDrivers.Lookup(sess.ID); found {
					d.cancelForPersistence(err)
				}
				if !session.IsTemporary(err) || ctx.Err() != nil {
					break
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				delay = min(2*delay, time.Second)
				err, attemptExpired = retry()
			}
			continue
		}
		obs.OnRunStart(ctx, sess)
	}
}

// observe forwards events after observeRunStart completes and execution begins.
func (r *LocalRuntime) observe(ctx context.Context, sess *session.Session, inner <-chan Event) <-chan Event {
	out := make(chan Event, cap(inner))
	go func() {
		defer close(out)
		for event := range inner {
			if fence, ok := event.(*observerDeliveryFence); ok {
				close(fence.done)
				continue
			}
			for _, obs := range r.observers {
				obs.OnEvent(ctx, sess, event)
				if persistence, ok := obs.(*PersistenceObserver); ok {
					if err := persistence.pendingError(sess.ID); err != nil {
						if d, found := r.sessionDrivers.Lookup(sess.ID); found {
							d.cancelForPersistence(err)
						}
					}
				}
			}
			// Publish through the owning session. Persistence observers and
			// attached views consume the same ordered transition.
			if d, ok := r.sessionDrivers.Lookup(sess.ID); ok {
				d.events.Publish(sess.ID, event)
			}
			out <- event
		}
	}()
	return out
}
