package toolexec

import (
	"context"
	"sync"
)

type invocationKey struct{}

// invocationPermit follows one call through synchronous nested execution.
// Sibling calls never share a permit, even when they share a session.
type invocationPermit struct {
	mu      sync.Mutex
	acquire func(context.Context) (func(), error)
	release func()
}

func (d *Dispatcher) invokeAdmitted(ctx context.Context, sessionID, toolName string, exec func(context.Context)) error {
	if d.AcquireTool == nil {
		exec(ctx)
		return nil
	}
	p := &invocationPermit{acquire: func(ctx context.Context) (func(), error) {
		return d.AcquireTool(ctx, sessionID, toolName)
	}}
	release, err := p.acquire(ctx)
	if err != nil {
		return err
	}
	p.release = release
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.release != nil {
			p.release()
			p.release = nil
		}
	}()
	exec(context.WithValue(ctx, invocationKey{}, p))
	return nil
}

// SuspendInvocation releases only the current coordinating call's permit.
// The returned function reacquires it before the call continues.
func SuspendInvocation(ctx context.Context) func(context.Context) error {
	p, _ := ctx.Value(invocationKey{}).(*invocationPermit)
	if p == nil {
		return func(context.Context) error { return nil }
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.release == nil {
		return func(context.Context) error { return nil }
	}
	p.release()
	p.release = nil
	return func(ctx context.Context) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.release != nil {
			return nil
		}
		release, err := p.acquire(ctx)
		if err != nil {
			return err
		}
		p.release = release
		return nil
	}
}
