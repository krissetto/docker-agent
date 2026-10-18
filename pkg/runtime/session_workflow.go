package runtime

import (
	"context"
	"errors"
	"time"
)

// SessionWorkflow is the optional synchronous host boundary for a session and
// its descendants. AwaitTurn continues to wait for only one accepted input.
type SessionWorkflow interface {
	AwaitWorkflow(ctx context.Context) (WorkflowSettlement, error)
}

// WorkflowSettlement fences output through the root journal's quiescent cursor.
type WorkflowSettlement struct {
	Epoch  string `json:"epoch"`
	Cursor uint64 `json:"cursor"`
}

func (h *sessionHandle) AwaitWorkflow(ctx context.Context) (WorkflowSettlement, error) {
	for {
		settlement, quiet, err := h.workflowSettlement(ctx)
		if err != nil || quiet {
			return settlement, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return WorkflowSettlement{}, ctx.Err()
		}
	}
}

func (h *sessionHandle) workflowSettlement(ctx context.Context) (WorkflowSettlement, bool, error) {
	m, g := h.runtime.subagents, h.runtime.sessionDrivers
	transition := m.transition(m.rootSessionLockedSafe(h.sessionID))
	if err := transition.LockContext(ctx); err != nil {
		return WorkflowSettlement{}, false, err
	}
	defer transition.Unlock()

	// The transition fence excludes child admission/completion and agent sends.
	// Copy topology before inspecting owners: owners can acquire manager.mu.
	ids := map[string]bool{h.sessionID: true}
	m.mu.Lock()
	for changed := true; changed; {
		changed = false
		for _, child := range m.children {
			if ids[child.parentSession] && !ids[child.sessionID] {
				ids[child.sessionID], changed = true, true
			}
		}
	}
	for _, admission := range m.admissions {
		if ids[admission.ParentSessionID] {
			m.mu.Unlock()
			return WorkflowSettlement{}, false, nil
		}
	}
	m.mu.Unlock()

	g.mu.Lock()
	drivers := make(map[string]*sessionDriver, len(ids))
	for id, driver := range g.drivers {
		drivers[id] = driver
	}
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return WorkflowSettlement{}, false, ErrSessionClosed
	}
	for changed := true; changed; {
		changed = false
		for id, driver := range drivers {
			if ids[driver.identityParent] && !ids[id] {
				ids[id], changed = true, true
			}
		}
	}

	// Read the outbox before owner state. AcceptReport can remove an outbox row
	// before its owner commits the pending input; ioReservations bridges that gap.
	for id := range ids {
		reports, err := m.coordination().PendingReports(ctx, id)
		if err != nil {
			return WorkflowSettlement{}, false, err
		}
		if len(reports) != 0 {
			return WorkflowSettlement{}, false, nil
		}
	}
	check := func(driver *sessionDriver) (bool, error) {
		quiet := false
		err := driver.ownerCall(ctx, func() error {
			if driver.completionErr != nil {
				return driver.completionErr
			}
			quiet = !driver.running() && !driver.starting() && !driver.settling() &&
				len(driver.pending) == 0 &&
				driver.ioReservations == 0 && driver.reportRetry == nil &&
				!driver.completionInFlight && !driver.retryRunning && driver.backgroundWorks == 0 &&
				driver.skillOperationID == "" && !driver.compactReserved &&
				!driver.switchReserved && !driver.editReserved
			return nil
		})
		if err != nil && driverDrained(driver) && (errors.Is(err, ErrSessionStopped) || errors.Is(err, ErrSessionClosed)) {
			return true, nil
		}
		return quiet, err
	}
	for id := range ids {
		if id == h.sessionID || drivers[id] == nil {
			continue
		}
		if quiet, err := check(drivers[id]); err != nil || !quiet {
			return WorkflowSettlement{}, false, err
		}
	}
	quiet, err := check(h.driver)
	if err != nil || !quiet {
		return WorkflowSettlement{}, false, err
	}
	h.driver.events.mu.Lock()
	settlement := WorkflowSettlement{Epoch: h.driver.events.epoch, Cursor: h.driver.events.nextSeq[h.sessionID]}
	h.driver.events.mu.Unlock()
	return settlement, true, nil
}
