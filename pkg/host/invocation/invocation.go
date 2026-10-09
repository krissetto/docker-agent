// Package invocation fences server-owned invocations before dependency teardown.
package invocation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrClosing = errors.New("server invocation lifetime is closing")

// DrainError retains the owner so callers can retry joining late finalization.
// A timeout does not stop workers or release their dependencies.
type DrainError struct {
	Err   error
	owner *Group
}

func (e *DrainError) Error() string                   { return fmt.Sprintf("drain server invocations: %v", e.Err) }
func (e *DrainError) Unwrap() error                   { return e.Err }
func (e *DrainError) Retry(ctx context.Context) error { return e.owner.retry(ctx) }

// Group owns invocation cancellation and a single dependency finalizer.
// Begin must bracket the actual worker, including settlement and owned supervisors,
// not merely the transport request that may launch it.
type Group struct {
	mu            sync.Mutex
	active        map[*byte]context.CancelFunc
	closing       bool
	drained       chan struct{}
	closed        *finalization
	cleanupErrors map[*byte]error
	finalize      func() error
}

type finalization struct {
	done chan struct{}
	err  error
}

// JoinCleanup retains the invocation until an owned supervisor has fully drained.
// Failed cleanup can be retried by the supervisor; it is not worker settlement.
func (g *Group) JoinCleanup(ctx context.Context, shutdown func(context.Context) error) {
	g.joinCleanup(ctx, shutdown, time.Sleep)
}

func (g *Group) joinCleanup(ctx context.Context, shutdown func(context.Context) error, delay func(time.Duration)) {
	ctx = context.WithoutCancel(ctx)
	key := new(byte)
	backoff := 100 * time.Millisecond
	for {
		err := shutdown(ctx)
		if g != nil {
			g.mu.Lock()
			if err == nil {
				delete(g.cleanupErrors, key)
			} else {
				g.cleanupErrors[key] = err
			}
			g.mu.Unlock()
		}
		if err == nil {
			return
		}
		delay(backoff)
		backoff = min(2*backoff, 5*time.Second)
	}
}

func New(finalize func() error) *Group {
	return &Group{active: make(map[*byte]context.CancelFunc), cleanupErrors: make(map[*byte]error), drained: make(chan struct{}), closed: &finalization{done: make(chan struct{})}, finalize: finalize}
}

func (g *Group) Begin(ctx context.Context) (context.Context, func(), error) {
	if g == nil {
		return ctx, func() {}, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing {
		return nil, nil, ErrClosing
	}
	ctx, cancel := context.WithCancel(ctx)
	key := new(byte)
	g.active[key] = cancel
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			g.mu.Lock()
			defer g.mu.Unlock()
			delete(g.active, key)
			if g.closing && len(g.active) == 0 {
				close(g.drained)
			}
		})
	}, nil
}

// Fence rejects new invocations and cancels accepted ones without joining them.
func (g *Group) Fence() {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return
	}
	g.closing = true
	cancels := make([]context.CancelFunc, 0, len(g.active))
	for _, cancel := range g.active {
		cancels = append(cancels, cancel)
	}
	if len(g.active) == 0 {
		close(g.drained)
	}
	g.startFinalizerLocked()
	g.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (g *Group) startFinalizerLocked() {
	closed := g.closed
	go func() {
		<-g.drained
		var err error
		if g.finalize != nil {
			err = g.finalize()
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		closed.err = err
		close(closed.done)
	}()
}

func (g *Group) retry(ctx context.Context) error {
	g.mu.Lock()
	select {
	case <-g.closed.done:
		if g.closed.err != nil {
			g.closed = &finalization{done: make(chan struct{})}
			g.startFinalizerLocked()
		}
	default:
	}
	g.mu.Unlock()
	return g.Close(ctx)
}

func (g *Group) Close(ctx context.Context) error {
	g.Fence()
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	select {
	case <-closed.done:
		err := closed.err
		if err != nil {
			return &DrainError{Err: err, owner: g}
		}
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		err := ctx.Err()
		for _, cleanupErr := range g.cleanupErrors {
			err = errors.Join(err, cleanupErr)
		}
		g.mu.Unlock()
		return &DrainError{Err: err, owner: g}
	}
}
