package runtime

import (
	"context"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/subagent"
)

type driverStopRequest struct {
	done     chan struct{}
	deleting atomic.Bool
	applied  atomic.Bool
	withdraw atomic.Bool
}

// Control requests have bounded, coalescing mailboxes independent of ordinary
// owner admission. Cancellation takes effect even while the owner is occupied.
func (d *sessionDriver) requestStop(deleting bool) bool {
	return d.requestStopWithIntent(deleting, false)
}

func (d *sessionDriver) requestStopWithIntent(deleting, withdraw bool) bool {
	var request *driverStopRequest
	for {
		previous := d.stopRequest.Load()
		if previous != nil && !previous.applied.Load() {
			request = previous
			if deleting {
				request.deleting.Store(true)
			}
			if withdraw {
				request.withdraw.Store(true)
			}
			if d.stopRequest.Load() == request && !request.applied.Load() {
				break
			}
			continue
		}
		next := &driverStopRequest{done: make(chan struct{})}
		next.deleting.Store(deleting)
		next.withdraw.Store(withdraw)
		if d.stopRequest.CompareAndSwap(previous, next) {
			request = next
			break
		}
	}
	state := d.registrySnapshot()
	state.cancelExecution()
	select {
	case d.stopWake <- struct{}{}:
	default:
	}
	return state.cancel != nil || state.compactCancel != nil
}

func (d *sessionDriver) awaitStop(ctx context.Context) error {
	request := d.stopRequest.Load()
	if request == nil {
		return nil
	}
	select {
	case <-request.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-d.ownerDone:
		return d.retiredOwnerError()
	}
}

func (d *sessionDriver) applyStopLocked() {
	request := d.stopRequest.Load()
	if request == nil {
		return
	}
	if request.deleting.Load() {
		d.events.FenceDelete(d.sessionIDLocked())
	}
	if request.applied.Load() {
		return
	}
	(driverRegistryState{cancel: d.cancel, compactCancel: d.compactCancel, skillCancel: d.skillCancel}).cancelExecution()
	if d.pauseCh != nil {
		close(d.pauseCh)
		d.pauseCh = nil
	}
	d.pending = nil
	d.stopped = true
	d.invalidateTitleLocked()
	d.stoppedView = false
	d.resolveInteractionsLocked()
	d.notifyTurnChangedLocked()
	d.skillGeneration++
	d.skillOperationID = ""
	d.skillCancel = nil
	d.signalStartDoneLocked()
	// Close joining before consuming intent; late joiners retain it in a new request.
	request.applied.Store(true)
	if request.withdraw.Swap(false) {
		d.durableStopRequested = true
	}
	close(request.done)
}

// Capture/enqueue is serialized by the manager; each owner consumes only its
// newest subtree, retaining root and nested-session journal ordering.
func (d *sessionDriver) enqueueTreeProjection(tree subagent.Snapshot) {
	d.treeProjection.Store(&tree)
	select {
	case d.treeWake <- struct{}{}:
	default:
	}
}

func (d *sessionDriver) applyTreeProjectionLocked() error {
	if tree := d.treeProjection.Swap(nil); tree != nil {
		d.sess.SetSubagentTree(tree)
		d.events.Publish(d.identityID, SubagentTree(*tree))
	}
	return nil
}
