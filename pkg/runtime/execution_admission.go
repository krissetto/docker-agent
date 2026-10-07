package runtime

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/subagent"
)

type executionLease struct {
	counted   bool
	suspended int
}

type executionAdmission struct {
	mu      sync.Mutex
	limit   int
	active  int
	leases  map[string]*executionLease
	changed chan struct{}
}

func newExecutionAdmission(limit int) *executionAdmission {
	return &executionAdmission{limit: limit, leases: make(map[string]*executionLease), changed: make(chan struct{})}
}

func (a *executionAdmission) notify() {
	close(a.changed)
	a.changed = make(chan struct{})
}

func (r *LocalRuntime) admission() *executionAdmission {
	r.executionAdmissionMu.Lock()
	defer r.executionAdmissionMu.Unlock()
	if r.executionAdmission == nil {
		r.executionAdmission = newExecutionAdmission(r.maxExecutions)
	}
	return r.executionAdmission
}

func (r *LocalRuntime) acquireExecution(ctx context.Context, sessionID string) (func(), error) {
	a := r.admission()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a.mu.Lock()
		if a.limit <= 0 {
			a.mu.Unlock()
			return nil, executionCapacityError(sessionID, "execution", a.limit)
		}
		if _, exists := a.leases[sessionID]; !exists && a.active < a.limit {
			lease := &executionLease{counted: true}
			a.leases[sessionID] = lease
			a.active++
			a.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					a.mu.Lock()
					defer a.mu.Unlock()
					if lease.counted {
						a.active--
					}
					delete(a.leases, sessionID)
					a.notify()
				})
			}, nil
		}
		changed := a.changed
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// suspendExecution lends the parent's capacity to canonical child executions.
func (r *LocalRuntime) suspendExecution(ctx context.Context, sessionID string) (func(context.Context) error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a := r.admission()
	a.mu.Lock()
	lease := a.leases[sessionID]
	if lease != nil {
		lease.suspended++
		if lease.counted {
			lease.counted = false
			a.active--
			a.notify()
		}
	}
	a.mu.Unlock()
	resumeTool := toolexec.SuspendInvocation(ctx)
	resumed := false
	return func(ctx context.Context) error {
		if lease != nil {
			a.mu.Lock()
			if !resumed {
				resumed = true
				lease.suspended--
				a.notify()
			}
			a.mu.Unlock()
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				a.mu.Lock()
				if a.leases[sessionID] != lease {
					a.mu.Unlock()
					return ErrSessionClosed
				}
				if lease.counted {
					a.mu.Unlock()
					break
				}
				if lease.suspended == 0 && a.active < a.limit {
					lease.counted = true
					a.active++
					a.mu.Unlock()
					break
				}
				changed := a.changed
				a.mu.Unlock()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-changed:
				}
			}
		}
		return resumeTool(ctx)
	}, nil
}

func executionCapacityError(sessionID string, operation SessionOperation, limit int) error {
	return &SessionError{Kind: SessionErrorCapacity, SessionID: sessionID, Operation: operation, Reason: SessionErrorReasonLimit, Limit: limit}
}

func (r *LocalRuntime) acquireTool(ctx context.Context, sessionID, toolName string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.maxTools <= 0 {
		return nil, executionCapacityError(sessionID, "tool", r.maxTools)
	}
	switch toolName {
	case "transfer_task", "handoff", subagent.ToolSpawnSubagent, subagent.ToolStopSubagent, subagent.ToolSendMessage, subagent.ToolReadSubagent:
		return func() {}, nil
	}
	select {
	case r.toolPermits <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-r.toolPermits }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
