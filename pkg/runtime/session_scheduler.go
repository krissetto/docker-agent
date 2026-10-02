package runtime

import (
	"slices"
	"sync"
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
		g.workStop = make(chan struct{})
		go g.schedule()
	})
	work := g.work
	g.mu.Unlock()
	select {
	case work <- struct{}{}:
	default:
	}
}

// Each resident driver has at most one maintenance attempt in flight. A slow
// store can stall that session, not the scheduler or an independent session.
func (g *sessionDriverRegistry) schedule() {
	var workers sync.WaitGroup
	dispatchDone := make(chan struct{})
	defer func() {
		close(dispatchDone)
		workers.Wait()
		close(g.workDone)
	}()
	type result struct {
		driver  *sessionDriver
		pending bool
	}
	results := make(chan result)
	busy := map[*sessionDriver]bool{}
	queued := map[*sessionDriver]bool{}
	var retry <-chan time.Time
	rotation := 0
	retryDelay := 100 * time.Millisecond
	// Bound outstanding storage attempts even when registry generations churn or
	// the session capacity is configured as unlimited.
	const maxMaintenanceWorkers = 32
	for {
		scan := false
		select {
		case <-g.r.lifetime().Done():
			return
		case <-g.workStop:
			return
		case done := <-results:
			delete(busy, done.driver)
			if done.pending && retry == nil {
				retry = time.After(retryDelay)
				retryDelay = min(2*retryDelay, time.Second)
			}
		case <-g.work:
			scan = true
			retryDelay = 100 * time.Millisecond
		case <-retry:
			retry = nil
			scan = true
		}
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			return
		}
		ids := make([]string, 0, len(g.drivers))
		resident := make(map[*sessionDriver]bool, len(g.drivers))
		for id, d := range g.drivers {
			ids = append(ids, id)
			resident[d] = true
			if scan {
				queued[d] = true
			}
		}
		// A signal arriving during I/O remains queued; completion cannot lose it.
		for d := range queued {
			if !resident[d] {
				delete(queued, d)
			}
		}
		slices.Sort(ids)
		if len(ids) > 0 {
			rotation %= len(ids)
			ids = append(ids[rotation:], ids[:rotation]...)
			rotation++
		}
		drivers := make([]*sessionDriver, 0, min(len(ids), maxMaintenanceWorkers))
		for _, id := range ids {
			d := g.drivers[id]
			if queued[d] && !busy[d] && len(busy)+len(drivers) < maxMaintenanceWorkers {
				if d.maintenanceRetired {
					delete(queued, d)
					continue
				}
				if !d.mu.TryLock() {
					if retry == nil {
						retry = time.After(retryDelay)
					}
					continue
				}
				if d.reclaiming {
					d.mu.Unlock()
					delete(queued, d)
					continue
				}
				// Reserve before releasing registry.mu: removal/close cannot observe a
				// drained driver and then race with a late worker Add.
				d.wg.Add(1)
				d.mu.Unlock()
				workers.Add(1)
				drivers = append(drivers, d)
				delete(queued, d)
			}
		}
		g.mu.Unlock()
		for _, d := range drivers {
			busy[d] = true
			go func() {
				defer workers.Done()
				pending := g.maintainDriver(d)
				d.wg.Done()
				select {
				case results <- result{d, pending}:
				case <-g.r.lifetime().Done():
				case <-dispatchDone:
				}
			}()
		}
	}
}

func (g *sessionDriverRegistry) maintainDriver(d *sessionDriver) bool {
	d.mu.Lock()
	dormant := d.viewDormant
	d.mu.Unlock()
	if dormant {
		return false
	}
	pending := g.deliverReports(d)
	d.mu.Lock()
	completion := d.settling() && d.completionErr != nil
	generation, runErr := d.generation, d.completionRunErr
	wake := d.pendingWakeableLocked() && !d.stopped && !d.running() && !d.starting() && !d.settling()
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
		pending = true
	}
	if wake {
		err := d.wakePending()
		d.mu.Lock()
		d.retryRunning = err != nil && isRetryableSessionError(err)
		pending = pending || d.retryRunning
		d.mu.Unlock()
	}
	return pending
}

// admitRun runs under runMu; capacity is derived from drivers, never tree state.
func (g *sessionDriverRegistry) admitRun(candidate *sessionDriver) error {
	if !candidate.identityAsync {
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
			if d.identityParent == "" {
				return id
			}
			id = d.identityParent
		}
		return id
	}
	rootID := root(candidate.identityParent)
	active, own := 0, 0
	for _, d := range g.drivers {
		if d == candidate {
			continue
		}
		if !d.identityAsync {
			continue
		}
		// Contended driver state may be doing storage I/O. Conservatively reserve
		// its slot for this attempt instead of holding the global admission lock.
		running := true
		if d.mu.TryLock() {
			running = d.running() || d.starting() || d.settling()
			d.mu.Unlock()
		}
		if !running {
			continue
		}

		active++
		if root(d.identityParent) == rootID {
			own++
		}
	}
	if !limitAllows(active, g.r.maxActiveDescendants) {
		return &SessionError{Kind: SessionErrorCapacity, SessionID: candidate.identityID, Operation: SessionOperationActiveDescendants, Limit: g.r.maxActiveDescendants}
	}
	if !limitAllows(own, g.r.maxActiveDescendantsRoot) {
		return &SessionError{Kind: SessionErrorCapacity, SessionID: candidate.identityID, Operation: SessionOperationActiveDescendantsRoot, Limit: g.r.maxActiveDescendantsRoot}
	}
	return nil
}
