package runtime

import (
	"time"

	"github.com/docker/docker-agent/pkg/session"
)

// turnClock measures one accepted root (human-initiated) turn on the runtime
// clock, from generation start to settlement. It is only touched inside owner
// calls. Waiting is the union of intervals during which at least one human
// interaction was open, so overlapping interactions are excluded once, and
// parallel tools or subagents count as elapsed wall time rather than sums.
type turnClock struct {
	turnID    string
	started   time.Time
	open      map[string]struct{}
	blockedAt time.Time
	waiting   time.Duration
	// ended is frozen at the first settlement attempt so that retried durable
	// writes report the same duration.
	ended time.Time
}

func (d *sessionDriver) turnClockNow() time.Time {
	if d.r != nil && d.r.now != nil {
		return d.r.now()
	}
	return time.Now()
}

// startTurnClockLocked begins timing only for user-originated turns. Agent and
// runtime inputs are not human-initiated, and identity-free runs have no turn to key.
func (d *sessionDriver) startTurnClockLocked(turnID string, origin session.InputOrigin) {
	if turnID == "" || normalizedInputOrigin(origin) != session.InputOriginUser {
		d.turnClock = nil
		return
	}
	d.turnClock = &turnClock{turnID: turnID, started: d.turnClockNow(), open: map[string]struct{}{}}
}

// openTurnWaitLocked marks an interaction as blocking on a human.
func (d *sessionDriver) openTurnWaitLocked(id string) {
	c := d.turnClock
	if c == nil || id == "" || !c.ended.IsZero() {
		return
	}
	if _, ok := c.open[id]; ok {
		return
	}
	if len(c.open) == 0 {
		c.blockedAt = d.turnClockNow()
	}
	c.open[id] = struct{}{}
}

// closeTurnWaitLocked ends an interaction wait. Unknown or already-closed IDs are no-ops.
func (d *sessionDriver) closeTurnWaitLocked(id string) {
	c := d.turnClock
	if c == nil {
		return
	}
	if _, ok := c.open[id]; !ok {
		return
	}
	delete(c.open, id)
	if len(c.open) == 0 {
		c.waiting += d.turnClockNow().Sub(c.blockedAt)
	}
}

// finishTurnClockLocked freezes the turn end and returns its timing. It returns
// false when no clock was started for turnID, so legacy or unmeasured turns get no timing.
func (d *sessionDriver) finishTurnClockLocked(turnID string, outcome TurnOutcome) (session.TurnTiming, bool) {
	c := d.turnClock
	if c == nil || c.turnID != turnID {
		return session.TurnTiming{}, false
	}
	if c.ended.IsZero() {
		now := d.turnClockNow()
		if len(c.open) > 0 {
			c.waiting += now.Sub(c.blockedAt)
			clear(c.open)
		}
		c.ended = now
	}
	active := c.ended.Sub(c.started) - c.waiting
	if active < 0 {
		active = 0
	}
	return session.TurnTiming{
		Version:    session.TurnTimingVersion,
		Basis:      session.TurnTimingBasisActiveWall,
		TurnID:     turnID,
		DurationMs: active.Milliseconds(),
		WaitingMs:  c.waiting.Milliseconds(),
		Outcome:    string(outcome),
	}, true
}

// setPersistedTurnTiming records timing for the settled turn and for steering
// inputs consumed into it, mirroring how terminal outcomes are recorded.
func setPersistedTurnTiming(s *session.Session, turnID string, consumed []string, timing session.TurnTiming) {
	for _, id := range append([]string{turnID}, consumed...) {
		if id == "" {
			continue
		}
		own := timing
		own.TurnID = id
		s.SetTurnTiming(id, own)
	}
}
