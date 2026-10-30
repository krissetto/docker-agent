package runtime

import (
	"context"
	"errors"
	"time"
)

// capacityReclaimWaitBudget bounds one capacity admission's total wait for the oldest
// eligible settled driver to become reclaimable. Driver-lock holds are state-only
// closures, and maintenance performs at most one store round trip per report, so
// transient contention clears well inside this budget.
const capacityReclaimWaitBudget = 250 * time.Millisecond

// supersededRetireWaitBudget bounds a same-ID replacement's wait for a stopped or
// reclaiming generation to finish retiring: a publication of the ID, and a view restore
// that must replace the ID.
//
// PROVISIONAL: 2s is the parent's current choice. It can be changed to 250ms by editing
// this constant alone. It is deliberately distinct from capacityReclaimWaitBudget,
// which bounds capacity-eviction waits.
const supersededRetireWaitBudget = 2 * time.Second

// capacityBusy describes transient contention that stopped reclaim of the oldest
// eligible settled driver. The oldest driver is never skipped while busy. done closes
// when the contended work finishes: maintenance drained, or the driver's retirement
// completed. A nil or already-closed done is paced by the timer alone.
type capacityBusy struct {
	driver *sessionDriver
	id     string
	done   <-chan struct{}
}

// capacityWaitError reports that an attempt released all of its locks because a
// driver was busy. The caller waits within the matching budget and reruns the attempt
// from scratch. capacity is the typed denial to return if that budget expires.
// Busy never authorizes evicting a younger driver.
type capacityWaitError struct {
	capacity error
	busy     *capacityBusy
	// superseded marks a wait for a same-ID generation, which uses supersededRetireWaitBudget.
	superseded bool
}

func (e *capacityWaitError) Error() string { return e.capacity.Error() }
func (e *capacityWaitError) Unwrap() error { return e.capacity }

// driverRetirement is one background retirement of a driver. done closes after the
// retirement finishes. err is the retirement's outcome, written under g.mu before done
// closes. A reclaim token that succeeded has also removed its driver from the registry.
type driverRetirement struct {
	id   string
	done chan struct{}
	// err is written under g.mu before done closes, and read only after done closes.
	err error
}

func retirementDone(r *driverRetirement) <-chan struct{} {
	if r == nil {
		return nil
	}
	return r.done
}

// startRetirementLocked returns the driver's single reclaim retirement, starting it when
// none is in flight. It is the only place a reclaim retirement begins, so d.retirement is
// the token that prevents duplicates. Caller holds g.mu. The retirement runs on its own
// goroutine with no lock held, and never inline. Close waits for it through retireWG,
// and no new retirement starts once the registry is closed.
func (g *sessionDriverRegistry) startRetirementLocked(id string, d *sessionDriver) *driverRetirement {
	if d.retirement != nil {
		return d.retirement
	}
	if g.closed {
		return nil
	}
	r := &driverRetirement{id: id, done: make(chan struct{})}
	d.retirement = r
	g.retiring++
	g.retireWG.Add(1)
	go g.runRetirement(d, r)
	return r
}

// runRetirement retires d and finishes the reclaim. The only lock it takes is g.mu,
// and only after the retirement returns. Defers run in reverse order, so g.mu is
// released before done closes.
//
// Removal completes here, and it may be refused. removeReclaimedLocked refuses when the
// ID is bound to another driver or a resident descendant still needs it. The driver then
// stays registered with its owner closed and reclaiming. Lookup hides such a driver and
// restarts this retirement, which returns at once because the owner is closed. The
// path that needs the ID waits for the completion, so removal is always finished or
// refused with a typed error, and no handle is handed out to a closed owner.
func (g *sessionDriverRegistry) runRetirement(d *sessionDriver, r *driverRetirement) {
	defer g.retireWG.Done()
	defer close(r.done)
	err := d.retire(context.WithoutCancel(g.r.lifetime()))
	g.mu.Lock()
	defer g.mu.Unlock()
	g.retiring--
	r.err = err
	if err != nil {
		d.lastRetireErr = err
	} else {
		d.lastRetireErr = nil
	}
	if d.retirement != r {
		return
	}
	d.retirement = nil
	if err == nil {
		g.removeReclaimedLocked(r.id, d)
	}
}

// capacityWaiter carries one budget and its backoff across the retries of one call.
type capacityWaiter struct {
	g        *sessionDriverRegistry
	deadline time.Time
	backoff  time.Duration
}

func (w *capacityWaiter) expired() bool { return !time.Now().Before(w.deadline) }

// busyWaiters holds one call's two budgets. Each deadline is fixed when the call starts
// and shared by all of its retries. Capacity waits and same-ID waits never share a budget.
type busyWaiters struct {
	capacity   *capacityWaiter
	superseded *capacityWaiter
}

func (g *sessionDriverRegistry) newBusyWaiters() *busyWaiters {
	budget := capacityReclaimWaitBudget
	if g.capacityWaitBudget > 0 {
		budget = g.capacityWaitBudget
	}
	now := time.Now()
	return &busyWaiters{
		capacity:   &capacityWaiter{g: g, deadline: now.Add(budget)},
		superseded: &capacityWaiter{g: g, deadline: now.Add(g.supersededRetireBudget())},
	}
}

// supersededRetireBudget returns the same-ID bound, honouring the registry override that
// tests set. Production never sets the override.
func (g *sessionDriverRegistry) supersededRetireBudget() time.Duration {
	if g.supersededRetireWaitOverride > 0 {
		return g.supersededRetireWaitOverride
	}
	return supersededRetireWaitBudget
}

// wait blocks for one backoff step, or until busy.done closes, the budget expires, ctx
// ends, or the runtime lifetime ends. Every call blocks on a timer whose duration is
// positive and bounded by the remaining budget, so retry loops cannot spin. Callers hold
// no registry, driver, publication, transition, or service lock. Expiry returns nil so
// the caller reruns its final attempt.
func (w *capacityWaiter) wait(ctx context.Context, busy *capacityBusy) error {
	if hook := w.g.capacityWaitHook.Load(); hook != nil {
		(*hook)()
	}
	remaining := time.Until(w.deadline)
	if remaining <= 0 {
		return nil
	}
	if w.backoff == 0 {
		w.backoff = subagentPersistenceRetryBase
	} else {
		w.backoff *= 2
	}
	timer := time.NewTimer(min(w.backoff, remaining))
	defer timer.Stop()
	var lifetime <-chan struct{}
	if w.g.r != nil {
		lifetime = w.g.r.lifetime().Done()
	}
	var done <-chan struct{}
	if busy.done != nil {
		select {
		case <-busy.done:
		default:
			done = busy.done
		}
	}
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	case <-lifetime:
		return ErrSessionClosed
	}
	return nil
}

// retryCapacityBusy reruns attempt while it reports transient busy state, under budgets
// fixed when the call starts. On expiry the final attempt's typed denial is returned.
// Attempts must release their own locks before returning a capacityWaitError.
func (g *sessionDriverRegistry) retryCapacityBusy(ctx context.Context, attempt func() error) error {
	return g.retryCapacityBusyWith(ctx, g.newBusyWaiters(), attempt)
}

// retryCapacityBusyWith is retryCapacityBusy with caller-owned budgets, so a pre-step and
// the retries share one set of deadlines.
func (g *sessionDriverRegistry) retryCapacityBusyWith(ctx context.Context, bw *busyWaiters, attempt func() error) error {
	for {
		err := attempt()
		busy, ok := errors.AsType[*capacityWaitError](err)
		if !ok {
			return err
		}
		w := bw.capacity
		if busy.superseded {
			w = bw.superseded
		}
		if w.expired() {
			if busy.superseded {
				return errors.Join(busy.capacity, context.DeadlineExceeded)
			}
			return busy.capacity
		}
		if waitErr := w.wait(ctx, busy.busy); waitErr != nil {
			return waitErr
		}
	}
}

// retirementStoppedError is the typed denial for a same-ID path that cannot proceed while
// the generation still retires. When a failed retirement is known, it is joined in, so
// errors.Is and errors.As reach both the stopped error and the cause.
func retirementStoppedError(id string, operation SessionOperation, detail string, last error) error {
	stopped := &SessionError{Kind: SessionErrorStopped, SessionID: id, Operation: operation, Detail: detail}
	if last == nil {
		return stopped
	}
	return errors.Join(stopped, last)
}

// retirementWaitExpired reports that a same-ID wait used up its internal budget while the
// generation was still retiring. The budget is a deadline, so DeadlineExceeded is joined in
// with the typed stopped error and the last retirement cause, if any. Caller cancellation
// never reaches here: the caller's own ctx error is returned instead.
func retirementWaitExpired(id string, operation SessionOperation, detail string, last error) error {
	return errors.Join(retirementStoppedError(id, operation, detail, last), context.DeadlineExceeded)
}

func retirementFinished(r *driverRetirement) bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// driverOwnerClosed reports whether d's owner has closed. The owner closes only after a
// successful retirement has stopped the driver's resources.
func driverOwnerClosed(d *sessionDriver) bool {
	select {
	case <-d.ownerDone:
		return true
	default:
		return false
	}
}

// abandonedRemovalLocked reports a reclaiming driver whose owner is closed: its retirement
// finished, but its removal was refused. Lookup hides it and resumes its removal.
func abandonedRemovalLocked(d *sessionDriver) bool {
	return d.registrySnapshot().reclaiming && driverOwnerClosed(d)
}

// replaceableGeneration reports whether a restore may take the place of d: stopped and
// settled, not reclaiming, and drained. Its owner may still be open. ActivateRestoreBatch
// drains the generation's resources before the replacement becomes visible, and it closes
// the owner after the swap, as before. A failed drain leaves the generation registered with
// its owner open, so authority and resources are retained.
func replaceableGeneration(d *sessionDriver) bool {
	snapshot := d.registrySnapshot()
	return snapshot.replaceable && !snapshot.reclaiming && driverDrained(d)
}

// supersessionLocked returns the stopped generation's single retirement, starting it when
// none is in flight or kept. Caller holds g.mu. The retirement runs on its own goroutine
// with no lock held. Its outcome is recorded under g.mu before done closes.
func (g *sessionDriverRegistry) supersessionLocked(id string, d *sessionDriver) *driverRetirement {
	if r := d.supersession; r != nil {
		return r
	}
	r := &driverRetirement{id: id, done: make(chan struct{})}
	d.supersession = r
	g.retiring++
	g.retireWG.Go(func() {
		err := d.retire(context.WithoutCancel(g.r.lifetime()))
		g.mu.Lock()
		g.retiring--
		r.err = err
		if err != nil {
			d.lastRetireErr = err
		} else {
			d.lastRetireErr = nil
		}
		g.mu.Unlock()
		close(r.done)
	})
	return r
}

// takeSupersessionLocked consumes a finished supersession token. A failed token is
// cleared, so the next publication starts a fresh retirement, and its error is returned
// as-is. Caller holds g.mu.
func (g *sessionDriverRegistry) takeSupersessionLocked(d *sessionDriver, r *driverRetirement) error {
	if d.supersession == r {
		d.supersession = nil
	}
	return r.err
}

// supersessionOutcomeLocked advances a superseded generation without waiting, for callers
// that hold registry locks. It always returns an error: the consumed failure, or the typed
// stopped error while retirement is still in flight. Caller holds g.mu.
func (g *sessionDriverRegistry) supersessionOutcomeLocked(id string, d *sessionDriver) error {
	r := g.supersessionLocked(id, d)
	if !retirementFinished(r) {
		return retirementStoppedError(id, "register", "superseded generation is still retiring", d.lastRetireErr)
	}
	if err := g.takeSupersessionLocked(d, r); err != nil {
		return err
	}
	return retirementStoppedError(id, "register", "superseded generation is still retiring", nil)
}

// awaitSupersededRetirement finishes the retirement that a publication of id needs before
// it may proceed. It runs before any publication or init shard is taken, and it waits only
// on supersededRetireBudget. A reclaiming generation, including an abandoned removal, is
// completed through its reclaim token. A stopped generation with an open owner is retired
// through its supersession token.
func (g *sessionDriverRegistry) awaitSupersededRetirement(ctx context.Context, id string) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return ErrSessionClosed
	}
	d := g.drivers[id]
	if d == nil {
		g.mu.Unlock()
		return nil
	}
	snapshot := d.registrySnapshot()
	if snapshot.reclaiming {
		r := g.startRetirementLocked(id, d)
		g.mu.Unlock()
		if r == nil {
			return ErrSessionClosed
		}
		return g.awaitReclaimRetirement(ctx, id, d, r)
	}
	if !snapshot.stopped || !snapshot.settled || driverOwnerClosed(d) {
		g.mu.Unlock()
		return nil
	}
	r := g.supersessionLocked(id, d)
	g.mu.Unlock()
	timer := time.NewTimer(g.supersededRetireBudget())
	defer timer.Stop()
	select {
	case <-r.done:
		g.mu.Lock()
		err := g.takeSupersessionLocked(d, r)
		g.mu.Unlock()
		return err
	case <-timer.C:
		g.mu.Lock()
		last := d.lastRetireErr
		g.mu.Unlock()
		return retirementWaitExpired(id, "register", "superseded generation is still retiring", last)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitReclaimRetirement waits for a reclaim token on the same-ID budget. A failed
// retirement returns its own error. A succeeded one has removed the driver, or the caller
// sees it still registered and gets its stopped error.
func (g *sessionDriverRegistry) awaitReclaimRetirement(ctx context.Context, id string, d *sessionDriver, r *driverRetirement) error {
	timer := time.NewTimer(g.supersededRetireBudget())
	defer timer.Stop()
	select {
	case <-r.done:
		g.mu.Lock()
		err := r.err
		g.mu.Unlock()
		return err
	case <-timer.C:
		g.mu.Lock()
		last := d.lastRetireErr
		g.mu.Unlock()
		return retirementWaitExpired(id, "register", "generation is still being reclaimed", last)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// generationWaitLocked reports what a restore must wait for before it may take the place
// of generation d for id. Caller holds g.mu. A reclaiming generation waits on its reclaim
// token, started if absent. A generation whose owner is open waits on its supersession
// token. A generation whose owner is closed needs no wait and returns (nil, nil).
func (g *sessionDriverRegistry) generationWaitLocked(id string, d *sessionDriver) (*capacityWaitError, error) {
	if d.registrySnapshot().reclaiming {
		r := g.startRetirementLocked(id, d)
		if r == nil {
			return nil, ErrSessionClosed
		}
		return &capacityWaitError{
			capacity:   retirementStoppedError(id, "prepare_view", "generation is still being reclaimed", d.lastRetireErr),
			busy:       &capacityBusy{driver: d, id: id, done: r.done},
			superseded: true,
		}, nil
	}
	if driverOwnerClosed(d) {
		return nil, nil
	}
	r := g.supersessionLocked(id, d)
	if !retirementFinished(r) {
		return &capacityWaitError{
			capacity:   retirementStoppedError(id, "prepare_view", "superseded generation is still retiring", d.lastRetireErr),
			busy:       &capacityBusy{driver: d, id: id, done: r.done},
			superseded: true,
		}, nil
	}
	return nil, g.takeSupersessionLocked(d, r)
}
