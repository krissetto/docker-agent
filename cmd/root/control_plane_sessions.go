package root

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type controlPlaneRuntime struct {
	id uint64
	rt runtime.SessionRuntime

	mu        sync.Mutex
	inflight  int
	draining  bool
	drained   chan struct{}
	drainOnce sync.Once
}

func newControlPlaneRuntime(id uint64, rt runtime.SessionRuntime) *controlPlaneRuntime {
	r := &controlPlaneRuntime{id: id, rt: rt, drained: make(chan struct{})}
	return r
}

func (r *controlPlaneRuntime) acquire() (func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.draining {
		return nil, false
	}
	r.inflight++
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.inflight--
		if r.inflight == 0 && r.draining {
			close(r.drained)
		}
	}, true
}

func (r *controlPlaneRuntime) startDrain() {
	r.drainOnce.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.draining = true
		if r.inflight == 0 {
			close(r.drained)
		}
	})
}

func (r *controlPlaneRuntime) waitDrain(ctx context.Context) error {
	select {
	case <-r.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// controlPlaneSessions is the session registry the --listen control plane serves.
// Add registrations must be unregistered before their owning runtime shuts down.
// Unregister removes discoverability then drains operations that already borrowed
// the registration. Handle Release remains unobservable through this interface.
type controlPlaneSessions struct {
	primary *controlPlaneRuntime

	mu     sync.RWMutex
	nextID uint64
	extras []*controlPlaneRuntime
	owners map[string]*controlPlaneRuntime
}

var _ interface {
	runtime.SessionRuntime
	runtime.TreeInspector
	runtime.TreeRestorer
	runtime.AgentSwitcher
	runtime.SafetyDefaults
} = (*controlPlaneSessions)(nil)

func newControlPlaneSessions(primary runtime.SessionRuntime) *controlPlaneSessions {
	return &controlPlaneSessions{primary: newControlPlaneRuntime(0, primary), owners: map[string]*controlPlaneRuntime{}}
}

// Add returns an idempotent unregister callback. It removes the registration
// from new lookups and waits for already-borrowed operations before returning.
func (c *controlPlaneSessions) Add(rt runtime.SessionRuntime) func(context.Context) error {
	if rt == nil {
		return func(context.Context) error { return nil }
	}
	c.mu.Lock()
	c.nextID++
	registration := newControlPlaneRuntime(c.nextID, rt)
	c.extras = append(c.extras, registration)
	c.mu.Unlock()
	var removeOnce sync.Once
	return func(ctx context.Context) error {
		removeOnce.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			for i, extra := range c.extras {
				if extra == registration {
					c.extras = append(c.extras[:i], c.extras[i+1:]...)
					break
				}
			}
			for sessionID, owner := range c.owners {
				if owner == registration {
					delete(c.owners, sessionID)
				}
			}
			registration.startDrain()
		})
		return registration.waitDrain(ctx)
	}
}

func (c *controlPlaneSessions) cache(sessionID string, owner *controlPlaneRuntime) {
	if sessionID == "" || owner == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if !owner.draining {
		c.owners[sessionID] = owner
	}
}

// owner returns a leased registration. No registry lock is held while runtime
// code executes; release must be called when the operation completes.
func (c *controlPlaneSessions) owner(sessionID string) (*controlPlaneRuntime, func()) {
	c.mu.RLock()
	cached := c.owners[sessionID]
	extras := append([]*controlPlaneRuntime(nil), c.extras...)
	c.mu.RUnlock()
	if cached != nil {
		if release, ok := cached.acquire(); ok {
			return cached, release
		}
	}
	candidates := append([]*controlPlaneRuntime{c.primary}, extras...)
	for _, candidate := range candidates {
		release, ok := candidate.acquire()
		if !ok {
			continue
		}
		if _, err := candidate.rt.SessionByID(sessionID); err == nil {
			c.cache(sessionID, candidate)
			return candidate, release
		}
		release()
	}
	release, _ := c.primary.acquire()
	return c.primary, release
}

func (c *controlPlaneSessions) CreateSession(ctx context.Context, sess *session.Session, binding runtime.SessionBinding) (runtime.SessionHandle, error) {
	if sess == nil {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, Operation: "create_session"}
	}
	id := sess.ID
	if binding.ParentSessionID != "" {
		id = binding.ParentSessionID
	}
	owner, release := c.owner(id)
	defer release()
	handle, err := owner.rt.CreateSession(ctx, sess, binding)
	if err == nil {
		c.cache(handle.ID(), owner)
	}
	return handle, err
}

func (c *controlPlaneSessions) SessionByID(sessionID string) (runtime.SessionHandle, error) {
	owner, release := c.owner(sessionID)
	defer release()
	if handle, err := owner.rt.SessionByID(sessionID); err == nil {
		c.cache(sessionID, owner)
		return handle, nil
	}
	return nil, &runtime.SessionError{Kind: runtime.SessionErrorNotFound, SessionID: sessionID, Operation: "lookup"}
}

func (c *controlPlaneSessions) DeleteSession(ctx context.Context, sessionID string) error {
	owner, release := c.owner(sessionID)
	defer release()
	if err := owner.rt.DeleteSession(ctx, sessionID); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.owners, sessionID)
	return nil
}

func (c *controlPlaneSessions) InspectSessionTree(ctx context.Context, rootSessionID string) (*subagent.Snapshot, error) {
	owner, release := c.owner(rootSessionID)
	defer release()
	inspector, ok := owner.rt.(runtime.TreeInspector)
	if !ok {
		return nil, &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: rootSessionID, Operation: "inspect_tree"}
	}
	return inspector.InspectSessionTree(ctx, rootSessionID)
}

func (c *controlPlaneSessions) RestoreSessionTree(ctx context.Context, root *session.Session) error {
	if root == nil {
		return &runtime.SessionError{Kind: runtime.SessionErrorInvalid, Operation: "restore_tree"}
	}
	owner, release := c.owner(root.ID)
	defer release()
	restorer, ok := owner.rt.(runtime.TreeRestorer)
	if !ok {
		return &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: root.ID, Operation: "restore_child_tree"}
	}
	if err := restorer.RestoreSessionTree(ctx, root); err != nil {
		return err
	}
	c.cache(root.ID, owner)
	return nil
}

func (c *controlPlaneSessions) SwitchAgent(ctx context.Context, sessionID, targetAgent string) (runtime.SessionHandle, *session.Session, error) {
	owner, release := c.owner(sessionID)
	defer release()
	switcher, ok := owner.rt.(runtime.AgentSwitcher)
	if !ok {
		return nil, nil, &runtime.SessionError{Kind: runtime.SessionErrorUnsupported, SessionID: sessionID, Operation: "switch_agent"}
	}
	handle, sess, err := switcher.SwitchAgent(ctx, sessionID, targetAgent)
	if err == nil {
		c.cache(handle.ID(), owner)
	}
	return handle, sess, err
}

func (c *controlPlaneSessions) AuthorSafetyDefault(sess *session.Session) session.SafetyPolicy {
	if defaults, ok := c.primary.rt.(runtime.SafetyDefaults); ok {
		return defaults.AuthorSafetyDefault(sess)
	}
	return ""
}
