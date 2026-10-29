package runtime

import (
	"context"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/subagent"
)

type driverStopRequest struct {
	done            chan struct{}
	deleting        atomic.Bool
	applied         atomic.Bool
	withdraw        atomic.Bool
	explicitStopped atomic.Bool
	// retiring marks an internal stop issued while the driver is being removed.
	retiring atomic.Bool
}

// Control requests have bounded, coalescing mailboxes independent of ordinary
// owner admission. Cancellation takes effect even while the owner is occupied.
func (d *sessionDriver) requestStop(deleting bool) bool {
	return d.requestStopWithIntent(deleting, false)
}

func (d *sessionDriver) requestStopWithIntent(deleting, withdraw bool) bool {
	return d.requestStopIntent(deleting, withdraw, false, false)
}

func (d *sessionDriver) requestStopWithExplicitStopped() bool {
	return d.requestStopIntent(false, false, true, false)
}

func (d *sessionDriver) requestStopIntent(deleting, withdraw, explicitStopped, retiring bool) bool {
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
			if explicitStopped {
				request.explicitStopped.Store(true)
			}
			if retiring {
				request.retiring.Store(true)
			}
			if d.stopRequest.Load() == request && !request.applied.Load() {
				break
			}
			continue
		}
		next := &driverStopRequest{done: make(chan struct{})}
		next.deleting.Store(deleting)
		next.withdraw.Store(withdraw)
		next.explicitStopped.Store(explicitStopped)
		next.retiring.Store(retiring)
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
	if request.applied.Load() {
		return
	}
	// Seal the joinable receipt before consuming its atomic intents. A late
	// joiner retries with a fresh request and wake rather than racing cleanup.
	request.applied.Store(true)
	if request.deleting.Load() {
		d.events.FenceDelete(d.sessionIDLocked())
	}
	// Dormancy is observable only for stops that leave the view alive; deletion
	// and internal retirement end it without a transient dormancy notification.
	suppressDormancy := request.deleting.Load() || request.retiring.Load()
	(driverRegistryState{cancel: d.cancel, compactCancel: d.compactCancel, skillCancel: d.skillCancel}).cancelExecution()
	if d.pauseCh != nil {
		close(d.pauseCh)
		d.pauseCh = nil
	}
	if !request.deleting.Load() {
		d.flushAllDeferredAcceptedLocked()
	}
	d.deferredAccepted = nil
	d.deferredAcceptedOrder = nil
	d.causalReportOnly = false
	d.causalReports = nil
	d.causalReportOrder = nil
	d.deferredCompletionReportTurnID = ""
	d.pending = nil
	d.stopped = true
	if request.withdraw.Swap(false) {
		d.durableStopRequested = true
		d.explicitStopped = true
	}
	if request.explicitStopped.Swap(false) {
		d.explicitStopped = true
	}
	if !suppressDormancy {
		d.events.Publish(d.sessionIDLocked(), &DormancyChangedEvent{AgentContext: newAgentContext(d.AgentNameLocked()), Type: "session_dormancy_changed", SessionID: d.sessionIDLocked(), Dormant: d.viewDormant, Stopped: d.explicitStopLocked()})
	}
	d.invalidateTitleLocked()
	d.stoppedView = false
	d.resolveInteractionsLocked()
	d.notifyTurnChangedLocked()
	d.skillGeneration++
	d.skillOperationID = ""
	d.skillCancel = nil
	d.signalStartDoneLocked()
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
