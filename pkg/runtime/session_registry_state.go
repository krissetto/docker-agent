package runtime

import "time"

type driverRegistryState struct {
	agentName   string
	stopped     bool
	settled     bool
	active      bool
	reclaiming  bool
	replaceable bool
	stoppedView bool
	lastActive  time.Time
}

func (d *sessionDriver) publishRegistryStateLocked() {
	state := &driverRegistryState{
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
