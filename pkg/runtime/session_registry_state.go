package runtime

import (
	"context"
	"time"
)

type driverRegistryState struct {
	cancel        context.CancelFunc
	compactCancel context.CancelFunc
	skillCancel   context.CancelFunc
	agentName     string
	stopped       bool
	settled       bool
	active        bool
	reclaiming    bool
	replaceable   bool
	stoppedView   bool
	lastActive    time.Time
}

func (d *sessionDriver) publishRegistryStateLocked() {
	if request := d.stopRequest.Load(); request != nil && request.withdraw.Swap(false) {
		d.durableStopRequested = true
	}
	state := &driverRegistryState{
		cancel: d.cancel, compactCancel: d.compactCancel, skillCancel: d.skillCancel,
		stopped:     d.stopped,
		settled:     !d.running() && !d.starting() && !d.settling(),
		active:      d.running() || d.starting() || d.settling(),
		reclaiming:  d.reclaiming,
		stoppedView: d.stoppedView,
		lastActive:  d.lastActive,
	}
	if d.sess != nil {
		state.agentName = d.sess.AgentName
	}
	state.replaceable = state.stopped && state.settled && !d.compactReserved && !d.switchReserved && !d.retryRunning && !d.completionInFlight
	d.registryState.Store(state)
	if request := d.stopRequest.Load(); request != nil && (!request.applied.Load() || d.stopped) {
		state.cancelExecution()
	}
}

func driverDrained(d *sessionDriver) bool {
	select {
	case <-d.Done():
		return true
	default:
		return false
	}
}

func (d *sessionDriver) registrySnapshot() driverRegistryState {
	if state := d.registryState.Load(); state != nil {
		return *state
	}
	return driverRegistryState{active: true}
}

func (s driverRegistryState) cancelExecution() {
	if s.cancel != nil {
		s.cancel()
	}
	if s.compactCancel != nil {
		s.compactCancel()
	}
	if s.skillCancel != nil {
		s.skillCancel()
	}
}
