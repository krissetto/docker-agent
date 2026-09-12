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
	closed   bool
}

type idleRuntime struct {
	owner runtime.SessionRuntimeSupervisor
	rt    runtime.SessionRuntime
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
func (p *runtimePool) Get(agent string) (runtime.SessionRuntime, func(context.Context) error, error) {
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
		p.mu.Unlock()
		_ = owner.Shutdown(p.ctx())
		return nil, nil, errInvalidRuntime
	}
	p.borrowed[entry] = struct{}{}
	p.mu.Unlock()
	return entry.rt, p.release(agent, entry), nil
}

func (p *runtimePool) release(agent string, entry *idleRuntime) func(context.Context) error {
	var once sync.Once
	var err error
	return func(ctx context.Context) error {
		once.Do(func() { err = p.put(ctx, agent, entry) })
		return err
	}
}

func (p *runtimePool) put(ctx context.Context, agent string, entry *idleRuntime) error {
	p.mu.Lock()
	if _, ok := p.borrowed[entry]; !ok {
		p.mu.Unlock()
		return nil
	}
	delete(p.borrowed, entry)
	if p.closed || p.maxIdle == 0 {
		p.mu.Unlock()
		return entry.owner.Shutdown(ctx)
	}
	idle := p.idle[agent]
	if idle == nil {
		idle = list.New()
		p.idle[agent] = idle
	}
	idle.PushBack(entry)
	if idle.Len() <= p.maxIdle {
		p.mu.Unlock()
		return nil
	}
	evicted := idle.Remove(idle.Front()).(*idleRuntime)
	p.mu.Unlock()
	return evicted.owner.Shutdown(ctx)
}

func (p *runtimePool) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	p.closed = true
	owners := make([]runtime.SessionRuntimeSupervisor, 0)
	for entry := range p.borrowed {
		owners = append(owners, entry.owner)
	}
	for _, idle := range p.idle {
		for elem := idle.Front(); elem != nil; elem = elem.Next() {
			owners = append(owners, elem.Value.(*idleRuntime).owner)
		}
	}
	p.idle = make(map[string]*list.List)
	p.borrowed = make(map[*idleRuntime]struct{})
	p.mu.Unlock()

	errs := make([]error, 0, len(owners))
	for _, owner := range owners {
		errs = append(errs, owner.Shutdown(ctx))
	}
	return errors.Join(errs...)
}
