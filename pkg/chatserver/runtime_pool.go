package chatserver

import (
	"container/list"
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/otel"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/version"
)

// runtimePool keeps a bounded set of idle session runtime supervisors per
// agent. A runtime is borrowed by one request at a time and returned after its
// ephemeral session has been released. The least recently returned runtime is
// shut down when an agent's idle limit is exceeded.
type runtimePool struct {
	team    *team.Team
	ctx     func() context.Context
	maxIdle int
	new     func() (runtime.SessionRuntimeSupervisor, error)

	mu       sync.Mutex
	idle     map[string]*list.List
	borrowed map[*idleRuntime]struct{}
	draining map[*idleRuntime]struct{}
	closed   bool
}

type idleRuntime struct {
	owner    runtime.SessionRuntimeSupervisor
	rt       runtime.SessionRuntime
	stopping chan struct{}
	stopped  bool
}

// errInvalidRuntime is returned when a caller asks for a runtime for an
// agent the pool can't create one for. Today this can only happen if
// runtime.New fails for a reason unrelated to the team (e.g. context
// cancellation in a future async path).
var errInvalidRuntime = errors.New("failed to acquire runtime")

func newRuntimePool(ctx context.Context, t *team.Team, maxIdle int) *runtimePool {
	if maxIdle < 0 {
		maxIdle = 0
	}
	p := &runtimePool{
		team:     t,
		ctx:      func() context.Context { return context.WithoutCancel(ctx) },
		maxIdle:  maxIdle,
		idle:     make(map[string]*list.List),
		borrowed: make(map[*idleRuntime]struct{}),
		draining: make(map[*idleRuntime]struct{}),
	}
	p.new = func() (runtime.SessionRuntimeSupervisor, error) {
		rt, err := runtime.NewLocalRuntime(p.ctx(), p.team,
			runtime.WithTracer(otel.Tracer(version.AppName)),
		)
		if err != nil {
			return nil, err
		}
		return runtime.NewSessionRuntimeSupervisor(rt), nil
	}
	return p
}

// Get returns a ready-to-use runtime for agent and a release function that
// must be called after the request's session has been released.
func (p *runtimePool) Get(agent string) (runtime.SessionRuntime, func(context.Context, bool) error, error) {
	if p == nil {
		return nil, nil, errInvalidRuntime
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, nil, errInvalidRuntime
	}
	if idle := p.idle[agent]; idle != nil && idle.Len() > 0 {
		elem := idle.Back()
		entry := elem.Value.(*idleRuntime)
		idle.Remove(elem)
		if idle.Len() == 0 {
			delete(p.idle, agent)
		}
		p.borrowed[entry] = struct{}{}
		p.mu.Unlock()
		return entry.rt, p.release(agent, entry), nil
	}
	p.mu.Unlock()

	owner, err := p.new()
	if err != nil {
		return nil, nil, err
	}
	entry := &idleRuntime{owner: owner, rt: owner.Runtime()}
	p.mu.Lock()
	if p.closed {
		p.draining[entry] = struct{}{}
		p.mu.Unlock()
		_ = p.drain(p.ctx(), entry)
		return nil, nil, errInvalidRuntime
	}
	p.borrowed[entry] = struct{}{}
	p.mu.Unlock()
	return entry.rt, p.release(agent, entry), nil
}

func (p *runtimePool) release(agent string, entry *idleRuntime) func(context.Context, bool) error {
	// Returning a lease happens once; draining its discarded/evicted owner is retryable.
	var once sync.Once
	var discarded *idleRuntime
	return func(ctx context.Context, reusable bool) error {
		once.Do(func() { discarded = p.put(agent, entry, reusable) })
		if discarded != nil {
			return p.drain(ctx, discarded)
		}
		return nil
	}
}

func (p *runtimePool) put(agent string, entry *idleRuntime, reusable bool) *idleRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.borrowed[entry]; !ok {
		if _, draining := p.draining[entry]; draining {
			return entry
		}
		return nil
	}
	delete(p.borrowed, entry)
	if !reusable || p.closed || p.maxIdle == 0 {
		p.draining[entry] = struct{}{}
		return entry
	}
	idle := p.idle[agent]
	if idle == nil {
		idle = list.New()
		p.idle[agent] = idle
	}
	idle.PushBack(entry)
	if idle.Len() <= p.maxIdle {
		return nil
	}
	evicted := idle.Remove(idle.Front()).(*idleRuntime)
	p.draining[evicted] = struct{}{}
	return evicted
}

// drain serializes attempts without making callers wait beyond their deadline.
// Failed owners stay reachable by Shutdown even if a lease is never retried.
func (p *runtimePool) drain(ctx context.Context, entry *idleRuntime) error {
	for {
		p.mu.Lock()
		if entry.stopped {
			p.mu.Unlock()
			return nil
		}
		if done := entry.stopping; done != nil {
			p.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		entry.stopping = make(chan struct{})
		p.mu.Unlock()
		err := entry.owner.Shutdown(ctx)
		p.mu.Lock()
		if err == nil {
			entry.stopped = true
			delete(p.draining, entry)
		}
		close(entry.stopping)
		entry.stopping = nil
		p.mu.Unlock()
		return err
	}
}

func (p *runtimePool) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.closed = true
	for entry := range p.borrowed {
		p.draining[entry] = struct{}{}
	}
	for _, idle := range p.idle {
		for elem := idle.Front(); elem != nil; elem = elem.Next() {
			p.draining[elem.Value.(*idleRuntime)] = struct{}{}
		}
	}
	p.idle = make(map[string]*list.List)
	p.borrowed = make(map[*idleRuntime]struct{})
	entries := make([]*idleRuntime, 0, len(p.draining))
	for entry := range p.draining {
		entries = append(entries, entry)
	}
	p.mu.Unlock()
	errs := make([]error, 0, len(entries))
	for _, entry := range entries {
		errs = append(errs, p.drain(ctx, entry))
	}
	return errors.Join(errs...)
}
