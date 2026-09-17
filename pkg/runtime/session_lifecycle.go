package runtime

import "context"

// sessionLifecycle owns the live generation phase. Durable child outcome is
// intentionally separate: retiring execution cannot invent a persisted stop.
type sessionPhase uint8

const (
	sessionIdle sessionPhase = iota
	sessionStarting
	sessionRunning
	sessionCancelling
	sessionSettling
)

type sessionLifecycle struct {
	phase             sessionPhase
	generation        uint64
	activeRequestID   string
	cancel            context.CancelFunc
	stopped           bool // execution retirement, not a durable child tombstone
	generationResult  string
	settledGeneration uint64
	// Settlement outcome captured when execution ends, not a live-phase flag.
	canceledOutcome bool
}

func (l *sessionLifecycle) running() bool {
	return l.phase == sessionRunning || l.phase == sessionCancelling
}
func (l *sessionLifecycle) starting() bool   { return l.phase == sessionStarting }
func (l *sessionLifecycle) settling() bool   { return l.phase == sessionSettling }
func (l *sessionLifecycle) cancelling() bool { return l.phase == sessionCancelling }
func (l *sessionLifecycle) leave(phase sessionPhase) {
	if l.phase == phase || (phase == sessionRunning && l.phase == sessionCancelling) {
		l.phase = sessionIdle
	}
}
func (l *sessionLifecycle) beginRun() { l.phase = sessionRunning; l.canceledOutcome = false }
