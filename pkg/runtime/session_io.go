package runtime

import "context"

// sessionIOLane permits one reserved durable effect, including its owner
// acknowledgement. A timed-out caller cannot release a still-running write.
type sessionIOLane struct {
	slot chan struct{}
}

func newSessionIOLane() *sessionIOLane {
	return &sessionIOLane{slot: make(chan struct{}, 1)}
}

type sessionIOReservation struct {
	write  func(context.Context) error
	commit func(error) error
}

// durableIO materializes payloads only after the preceding acknowledgement.
// reserve and commit run on the owner; write runs on the bounded I/O worker.
func (d *sessionDriver) durableIO(ctx context.Context, reserve func() (sessionIOReservation, error)) error {
	return d.durableIOContext(ctx, reserve, false)
}

func (d *sessionDriver) durableIOContext(ctx context.Context, reserve func() (sessionIOReservation, error), waitForAcknowledgement bool) error {
	var lane *sessionIOLane
	if err := d.ownerCall(ctx, func() error {
		if d.ioLane == nil {
			d.ioLane = newSessionIOLane()
		}
		lane = d.ioLane
		return nil
	}); err != nil {
		return err
	}
	select {
	case lane.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	var reservation sessionIOReservation
	if err := d.ownerCall(context.WithoutCancel(ctx), func() (err error) {
		if err := ctx.Err(); err != nil {
			return err
		}
		reservation, err = reserve()
		return err
	}); err != nil {
		<-lane.slot
		return err
	}
	done := make(chan error, 1)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() { <-lane.slot }()
		var err error
		if reservation.write != nil {
			err = reservation.write(ctx)
		}
		// Storage may have committed despite caller cancellation. Reconciliation
		// must finish before another write can acquire this lane.
		commitErr := d.ownerCall(context.WithoutCancel(ctx), func() error {
			if reservation.commit != nil {
				return reservation.commit(err)
			}
			return err
		})
		// Explicit stop is durable even when its caller timed out while this
		// worker retained the lane. Reconcile accepted late writes before release.
		var withdrawal sessionIOReservation
		withdrawalErr := d.ownerCall(context.WithoutCancel(ctx), func() error {
			if d.durableStopRequested {
				var err error
				withdrawal, err = d.reserveStoppedWithdrawal()
				return err
			}
			return nil
		})
		if withdrawalErr == nil && withdrawal.write != nil {
			stopCtx, cancel := context.WithTimeout(d.r.durabilityContext(), defaultSubagentPersistenceTimeout)
			withdrawalErr = withdrawal.write(stopCtx)
			cancel()
		}
		if withdrawal.commit != nil {
			withdrawalErr = d.ownerCall(context.WithoutCancel(ctx), func() error { return withdrawal.commit(withdrawalErr) })
		}
		if commitErr == nil {
			commitErr = withdrawalErr
		}
		done <- commitErr
	}()
	if waitForAcknowledgement {
		return <-done
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
