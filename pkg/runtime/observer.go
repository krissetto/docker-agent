package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

// EventObserver is a best-effort projection/telemetry boundary, not persistence
// or policy authority. Callbacks run on the observation worker, in registration
// order, with individually detached session/event snapshots. Mutation cannot
// change execution or another observer's input. Panics are logged and contained.
// Slow callbacks backpressure observation, not session mutation authority.
// Callbacks must honor ctx cancellation; worker joins do not abandon callbacks.
type EventObserver interface {
	// OnRunStart fires once when [LocalRuntime.RunStream] begins, before
	// any event is dispatched. Use it for one-shot projection setup; it cannot change session metadata.
	OnRunStart(ctx context.Context, sess *session.Session)
	// OnEvent fires once per event, after the runtime emits it but
	// before the consumer's channel receives it. Observers cannot
	// modify or suppress the authoritative payload or affect persistence.
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
		snapshot := sess.Clone()
		done := make(chan struct{})
		go func() {
			defer close(done)
			observeBestEffort(ctx, func() { obs.OnRunStart(ctx, snapshot) })
		}()
		<-done
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
			if identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity); ok {
				if err := identity.driver.commitExecutionEvent(context.WithoutCancel(ctx), identity.generation, event); err != nil {
					continue
				}
			}
			for _, obs := range r.observers {
				if persistence, ok := obs.(*PersistenceObserver); ok {
					// Storage is authoritative: it receives the owner payload and
					// failures remain visible to the driver's completion barrier.
					persistence.OnEvent(ctx, sess, event)
					if err := persistence.pendingError(sess.ID); err != nil {
						if d, found := r.sessionDrivers.Lookup(sess.ID); found {
							d.cancelForPersistence(err)
						}
					}
					continue
				}
				snapshot, err := observerEventSnapshot(event)
				if err != nil {
					slog.WarnContext(ctx, "Skipping observer event snapshot", "error", err)
					continue
				}
				observeBestEffort(ctx, func() { obs.OnEvent(ctx, sess.Clone(), snapshot) })
			}
			// Publish through the owning session. Persistence observers and
			// attached views consume the same ordered transition.
			if d, ok := r.sessionDrivers.Lookup(sess.ID); ok {
				if elicitation, ok := event.(*ElicitationRequestEvent); !ok || !elicitation.ownerPublished {
					d.events.Publish(sess.ID, event)
				}
			}
			out <- event
		}
	}()
	return out
}

func observeBestEffort(ctx context.Context, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			slog.WarnContext(ctx, "Event observer panicked", "panic", p)
		}
	}()
	fn()
}

func observerEventSnapshot(event Event) (Event, error) {
	t := reflect.TypeOf(event)
	if t == nil || t.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("unsupported observer event %T", event)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	copy := reflect.New(t.Elem()).Interface()
	if err := json.Unmarshal(data, copy); err != nil {
		return nil, err
	}
	out, ok := copy.(Event)
	if !ok {
		return nil, fmt.Errorf("unsupported observer event %T", event)
	}
	// Preserve process-local timestamp identity (including its monotonic clock).
	originalContext := reflect.ValueOf(event).Elem().FieldByName("AgentContext")
	copiedContext := reflect.ValueOf(out).Elem().FieldByName("AgentContext")
	if originalContext.IsValid() && copiedContext.IsValid() && copiedContext.CanSet() {
		copiedContext.Set(originalContext)
	}
	// Tool metadata and execution flags are intentionally absent from wire JSON.
	switch e := event.(type) {
	case *PartialToolCallEvent:
		if e.ToolDefinition != nil {
			tool := cloneLiveToolDefinition(*e.ToolDefinition)
			out.(*PartialToolCallEvent).ToolDefinition = &tool
		}
	case *ToolCallEvent:
		out.(*ToolCallEvent).ToolDefinition = cloneLiveToolDefinition(e.ToolDefinition)
	case *ToolCallConfirmationEvent:
		out.(*ToolCallConfirmationEvent).ToolDefinition = cloneLiveToolDefinition(e.ToolDefinition)
	case *ToolCallOutputEvent:
		out.(*ToolCallOutputEvent).ToolDefinition = cloneLiveToolDefinition(e.ToolDefinition)
	case *ToolCallResponseEvent:
		out.(*ToolCallResponseEvent).ToolDefinition = cloneLiveToolDefinition(e.ToolDefinition)
	case *HookBlockedEvent:
		out.(*HookBlockedEvent).ToolDefinition = cloneLiveToolDefinition(e.ToolDefinition)
	}
	switch e := event.(type) {
	case *MessageAddedEvent:
		copy := out.(*MessageAddedEvent)
		copy.boundaryOnly = e.boundaryOnly
		if e.Message != nil {
			data, err := json.Marshal(e.Message)
			if err != nil {
				return nil, err
			}
			copy.Message = &session.Message{}
			if err := json.Unmarshal(data, copy.Message); err != nil {
				return nil, err
			}
		}
	case *SessionSummaryEvent:
		out.(*SessionSummaryEvent).persisted = e.persisted
	}
	if e, ok := event.(*SubSessionCompletedEvent); ok {
		if child, ok := e.SubSession.(*session.Session); ok {
			out.(*SubSessionCompletedEvent).SubSession = child.Clone()
		}
	}
	return out, nil
}
