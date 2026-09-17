package runtime

import (
	"slices"
	"time"
)

// The registry schedules detached work. Drivers alone admit and execute turns;
// a coalesced signal and retry timer replace per-session and topology workers.
func (g *sessionDriverRegistry) signalWork() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.workOnce.Do(func() {
		g.work = make(chan struct{}, 1)
		g.workDone = make(chan struct{})
		go g.schedule()
	})
	work := g.work
	g.mu.Unlock()
	select {
	case work <- struct{}{}:
	default:
	}
}

func (g *sessionDriverRegistry) schedule() {
	defer close(g.workDone)
	var retry <-chan time.Time
	rotation := 0
	retryDelay := 100 * time.Millisecond
	for {
		select {
		case <-g.r.lifetime().Done():
			return
		case <-g.work:
			retryDelay = 100 * time.Millisecond
		case <-retry:
		}
		retry = nil
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return
		}
		ids := make([]string, 0, len(g.drivers))
		for id := range g.drivers {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		drivers := make([]*sessionDriver, 0, len(ids))
		if len(ids) > 0 {
			rotation %= len(ids)
			ids = append(ids[rotation:], ids[:rotation]...)
			rotation++
		}
		for _, id := range ids {
			drivers = append(drivers, g.drivers[id])
		}
		g.mu.Unlock()
		pendingWork := false
		for _, d := range drivers {
			d.mu.Lock()
			dormant := d.viewDormant
			d.mu.Unlock()
			if dormant {
				continue
			}
			pendingWork = g.deliverReports(d) || pendingWork
			d.mu.Lock()
			completion := d.settling && d.completionErr != nil && !d.stopped
			generation, runErr := d.generation, d.completionRunErr
			wake := len(d.pending) > 0 && !d.stopped && !d.running && !d.starting && !d.settling
			if completion {
				d.wg.Add(1)
			}
			d.mu.Unlock()
			if completion {
				ctx, next, again := d.finishRun(generation, runErr)
				if again {
					d.startWake(ctx, next)
				} else {
					d.wg.Done()
				}
				pendingWork = true
			}
			if wake {
				err := d.wakePending()
				d.mu.Lock()
				d.retryRunning = err != nil && isRetryableSessionError(err)
				pendingWork = pendingWork || d.retryRunning
				d.mu.Unlock()
			}
		}
		if pendingWork {
			retry = time.After(retryDelay)
			retryDelay = min(2*retryDelay, time.Second)
		} else {
			retryDelay = 100 * time.Millisecond
		}
	}
}

// admitRun runs under runMu; capacity is derived from drivers, never tree state.
func (g *sessionDriverRegistry) admitRun(candidate *sessionDriver) error {
	sess := candidate.session()
	if sess == nil || !sess.AsyncSubagent {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	root := func(id string) string {
		seen := map[string]bool{}
		for !seen[id] {
			seen[id] = true
			d := g.drivers[id]
			if d == nil {
				return id
			}
			s := d.session()
			if s == nil || s.ParentID == "" {
				return id
			}
			id = s.ParentID
		}
		return id
	}
	rootID := root(sess.ParentID)
	active, own := 0, 0
	for _, d := range g.drivers {
		if d == candidate {
			continue
		}
		d.mu.Lock()
		running := d.running || d.starting || d.settling
		s := d.sess
		d.mu.Unlock()
		if !running || s == nil || !s.AsyncSubagent {
			continue
		}
		active++
		if root(s.ParentID) == rootID {
			own++
		}
	}
	if !limitAllows(active, g.r.maxActiveDescendants) {
		return &SessionError{Kind: SessionErrorCapacity, SessionID: sess.ID, Operation: SessionOperationActiveDescendants, Limit: g.r.maxActiveDescendants}
	}
	if !limitAllows(own, g.r.maxActiveDescendantsRoot) {
		return &SessionError{Kind: SessionErrorCapacity, SessionID: sess.ID, Operation: SessionOperationActiveDescendantsRoot, Limit: g.r.maxActiveDescendantsRoot}
	}
	return nil
}
