package runtime

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/docker/docker-agent/pkg/session"
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
	return slices.ContainsFunc(d.pending, boundarySteering)
}

func boundarySteering(msg QueuedMessage) bool {
	return msg.RequestID == "" || msg.trustedSteering() || msg.activeSteering
}

func (d *sessionDriver) refreshSteeringLocked() {
	interrupts := func(msg QueuedMessage) bool {
		return normalizedInputOrigin(msg.InputOrigin) != session.InputOriginRuntime
	}
	d.interruptRequested = slices.ContainsFunc(d.steering, interrupts) || slices.ContainsFunc(d.pending, func(msg QueuedMessage) bool {
		return boundarySteering(msg) && interrupts(msg)
	})
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
	var signal chan struct{}
	_ = d.ownerCall(ctx, func() error {
		d.refreshSteeringLocked()
		signal = d.steeringChanged
		return nil
	})
	return context.WithValue(ctx, steeringBoundaryContextKey{}, signal)
}

// Promotion is one mailbox transaction across origins; ordinary turns stay queued.
func (d *sessionDriver) drainBoundarySteering() []QueuedMessage {
	return d.drainInputs(d.r.lifetime(), false)
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
