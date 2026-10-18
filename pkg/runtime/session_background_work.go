package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// sessionBackgroundWork holds the issuing team's environment and keeps
// workflow settlement open until its completion guidance has been admitted.
type sessionBackgroundWork struct {
	runtime     *LocalRuntime
	driver      *sessionDriver
	sessionID   string
	requestID   string
	workID      string
	generation  uint64
	environment *TeamEnvironmentLease

	closeOnce sync.Once
	cancelMu  sync.Mutex
	cancel    func()
	canceled  bool
	closeErr  error
}

func (r *LocalRuntime) beginSessionBackgroundWork(ctx context.Context, sess *session.Session, driver *sessionDriver) (tools.BackgroundWork, error) {
	if ctx == nil || ctx.Err() != nil || sess == nil || driver == nil || sess.ID != driver.sessionID() {
		return nil, ErrSessionClosed
	}
	identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity)
	if !ok || identity.driver != driver {
		return nil, errors.New("background work must be registered by its issuing session execution")
	}
	var lease *TeamEnvironmentLease
	if r.teamEnvironments != nil {
		if identity.environment == nil {
			return nil, errors.New("background work has no pinned team environment")
		}
		var err error
		lease, err = r.teamEnvironments.Acquire(identity.environment.Environment().RevisionID())
		if err != nil {
			return nil, err
		}
	}
	requestID, err := newSessionRequestID()
	if err != nil {
		if lease != nil {
			_ = lease.Release(context.WithoutCancel(r.lifetime()))
		}
		return nil, err
	}
	work := &sessionBackgroundWork{runtime: r, driver: driver, sessionID: sess.ID, requestID: requestID, workID: requestID, generation: identity.generation, environment: lease}
	if err := driver.ownerCall(context.WithoutCancel(r.lifetime()), func() error {
		if driver.stopped || driver.sess == nil || driver.sess.ID != sess.ID || driver.generation != identity.generation || !driver.running() {
			return ErrSessionClosed
		}
		driver.backgroundWorks++
		if driver.backgroundWorkCancelRequested == nil {
			driver.backgroundWorkCancelRequested = make(map[uint64]bool)
		}
		if driver.backgroundWorkCancel == nil {
			driver.backgroundWorkCancel = make(map[uint64]map[string]func())
		}
		if driver.backgroundWorkCancel[identity.generation] == nil {
			driver.backgroundWorkCancel[identity.generation] = make(map[string]func())
		}
		driver.backgroundWorkCancel[identity.generation][requestID] = nil
		return nil
	}); err != nil {
		if lease != nil {
			_ = lease.Release(context.WithoutCancel(r.lifetime()))
		}
		return nil, err
	}
	return work, nil
}

func (w *sessionBackgroundWork) Complete(message string) error {
	if message == "" {
		return errors.New("background work completion message is empty")
	}
	for {
		w.cancelMu.Lock()
		canceled := w.canceled
		w.cancelMu.Unlock()
		if canceled {
			return nil
		}
		if current, ok := w.runtime.sessionDrivers.Lookup(w.sessionID); !ok || current != w.driver {
			return ErrSessionClosed
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(w.runtime.lifetime()), defaultSubagentPersistenceTimeout)
		_, err := w.driver.postSteer(ctx, QueuedMessage{Content: message, RequestID: w.requestID, InputOrigin: session.InputOriginRuntime, InputMode: "steer"})
		cancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrSessionClosed) || errors.Is(err, ErrSessionStopped) {
			return err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-w.runtime.lifetime().Done():
			timer.Stop()
			return ErrSessionClosed
		}
	}
}

func (w *sessionBackgroundWork) Cancel() {
	w.cancelMu.Lock()
	w.canceled = true
	cancel := w.cancel
	w.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *sessionBackgroundWork) SetCancel(cancel func()) {
	w.cancelMu.Lock()
	w.cancel = cancel
	canceled := w.canceled
	w.cancelMu.Unlock()
	var cancelRequested bool
	err := w.driver.ownerCall(context.WithoutCancel(w.runtime.lifetime()), func() error {
		if w.driver.backgroundWorkCancel[w.generation] == nil {
			w.driver.backgroundWorkCancel[w.generation] = make(map[string]func())
		}
		w.driver.backgroundWorkCancel[w.generation][w.workID] = cancel
		cancelRequested = w.driver.backgroundWorkCancelRequested[w.generation]
		return nil
	})
	if err == nil && (canceled || cancelRequested) && cancel != nil {
		cancel()
	}
}

func (w *sessionBackgroundWork) Close() error {
	w.closeOnce.Do(func() {
		ctx := context.WithoutCancel(w.runtime.lifetime())
		w.closeErr = w.driver.ownerCall(ctx, func() error {
			delete(w.driver.backgroundWorkCancel[w.generation], w.workID)
			if len(w.driver.backgroundWorkCancel[w.generation]) == 0 {
				delete(w.driver.backgroundWorkCancel, w.generation)
				delete(w.driver.backgroundWorkCancelRequested, w.generation)
			}
			if w.driver.backgroundWorks <= 0 {
				return fmt.Errorf("session background-work count underflow for generation %d", w.generation)
			}
			w.driver.backgroundWorks--
			return nil
		})
		if w.environment != nil {
			w.closeErr = errors.Join(w.closeErr, w.environment.Release(ctx))
		}
	})
	return w.closeErr
}

func (d *sessionDriver) cancelBackgroundWorks(generation uint64) {
	var cancel []func()
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		if d.backgroundWorkCancel == nil || len(d.backgroundWorkCancel[generation]) == 0 {
			return nil
		}
		if d.backgroundWorkCancelRequested == nil {
			d.backgroundWorkCancelRequested = make(map[uint64]bool)
		}
		d.backgroundWorkCancelRequested[generation] = true
		for _, fn := range d.backgroundWorkCancel[generation] {
			if fn != nil {
				cancel = append(cancel, fn)
			}
		}
		return nil
	})
	for _, fn := range cancel {
		fn()
	}
}

func (d *sessionDriver) cancelAllBackgroundWorks() {
	var cancel []func()
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		if d.backgroundWorkCancel == nil {
			return nil
		}
		if d.backgroundWorkCancelRequested == nil {
			d.backgroundWorkCancelRequested = make(map[uint64]bool)
		}
		for generation, works := range d.backgroundWorkCancel {
			d.backgroundWorkCancelRequested[generation] = true
			for _, fn := range works {
				if fn != nil {
					cancel = append(cancel, fn)
				}
			}
		}
		return nil
	})
	for _, fn := range cancel {
		fn()
	}
}
