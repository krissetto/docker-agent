package runtime

import (
	"context"
	"errors"
	"slices"
	"time"
)

var errSteeringBoundary = errors.New("provider attempt interrupted by steering")

type steeringBoundaryContextKey struct{}

type observerDeliveryFence struct {
	AgentContext

	done chan struct{}
}

func waitForObserverDelivery(ctx context.Context, events EventSink) bool {
	if !observerDeliveryEnabled(ctx) {
		return ctx.Err() == nil
	}
	fence := &observerDeliveryFence{done: make(chan struct{})}
	if channel, ok := events.(*channelSink); ok {
		select {
		case channel.ch <- fence:
		case <-ctx.Done():
			return false
		}
	} else {
		events.Emit(fence)
	}
	select {
	case <-fence.done:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

type observerDeliveryContextKey struct{}

func observerDeliveryEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(observerDeliveryContextKey{}).(bool)
	return enabled
}

func steeringSignal(ctx context.Context) <-chan struct{} {
	signal, _ := ctx.Value(steeringBoundaryContextKey{}).(chan struct{})
	return signal
}

func (d *sessionDriver) hasSteeringLocked() bool {
	if len(d.steering) != 0 {
		return true
	}
	return slices.ContainsFunc(d.pending, func(msg QueuedMessage) bool {
		return msg.RequestID == "" || msg.trustedSteering()
	})
}

func (d *sessionDriver) refreshSteeringLocked() {
	d.interruptRequested = d.hasSteeringLocked()
	if d.steeringChanged == nil {
		d.steeringChanged = make(chan struct{})
	}
	select {
	case <-d.steeringChanged:
		if !d.interruptRequested {
			d.steeringChanged = make(chan struct{})
		}
	default:
		if d.interruptRequested {
			close(d.steeringChanged)
		}
	}
}

func (d *sessionDriver) steeringContext(ctx context.Context) context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refreshSteeringLocked()
	return context.WithValue(ctx, steeringBoundaryContextKey{}, d.steeringChanged)
}

// Promotion is one mailbox transaction across origins; ordinary turns stay queued.
func (d *sessionDriver) drainBoundarySteering() []QueuedMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	batch := slices.Clone(d.steering)
	for _, msg := range d.pending {
		if msg.RequestID == "" || msg.trustedSteering() {
			batch = append(batch, msg)
		}
	}
	slices.SortStableFunc(batch, func(a, b QueuedMessage) int { return a.AcceptedPosition - b.AcceptedPosition })
	promoted := make([]QueuedMessage, 0, len(batch))
	for _, msg := range batch {
		if msg.RequestID != "" {
			if err := d.promoteInputLocked(msg); err != nil {
				d.lastError = err.Error()
				break
			}
		}
		promoted = append(promoted, msg)
		remove := func(queued QueuedMessage) bool {
			return queued.RequestID == msg.RequestID && queued.Content == msg.Content && queued.AcceptedPosition == msg.AcceptedPosition
		}
		d.steering = slices.DeleteFunc(d.steering, remove)
		d.pending = slices.DeleteFunc(d.pending, remove)
	}
	d.refreshSteeringLocked()
	if len(promoted) != 0 {
		d.notifyTurnChangedLocked()
	}
	return promoted
}

func waitForRetryBoundary(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-steeringSignal(ctx):
		return errSteeringBoundary
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
