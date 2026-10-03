package runtime

import (
	"context"
	"errors"
	"sync"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
)

// SessionService owns process-local session routing and runtime lifetimes.
// Stores remain caller-owned. Claims are exclusive across services in this
// process; they are not distributed leases or durable ownership records.
type SessionService struct {
	mu          sync.Mutex
	runtimes    []*LocalRuntime
	closed      bool
	maxSessions int
}

// SessionServiceOptions bounds aggregate resident and reserved identities across
// this service's runtimes. Zero disables admission; -1 removes this bound.
// Other resource policy limits remain per runtime.
type SessionServiceOptions struct{ MaxSessions int }

var processSessions = struct {
	sync.Mutex

	owners map[string]*LocalRuntime
}{owners: make(map[string]*LocalRuntime)}

func NewSessionService(options ...SessionServiceOptions) *SessionService {
	limit := DefaultSessionResourcePolicy().MaxSessions
	if len(options) != 0 {
		limit = options[0].MaxSessions
	}
	return &SessionService{maxSessions: limit}
}

func WithSessionService(service *SessionService) Opt {
	return func(r *LocalRuntime) { r.sessionService = service }
}

// NewRuntime constructs an execution runtime whose sessions belong to this service.
func (s *SessionService) NewRuntime(ctx context.Context, agents *team.Team, opts ...Opt) (*LocalRuntime, error) {
	opts = append(opts, WithSessionService(s))
	return NewLocalRuntime(ctx, agents, opts...)
}

func (s *SessionService) register(r *LocalRuntime) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &SessionError{Kind: SessionErrorClosed, Operation: "register_runtime"}
	}
	s.runtimes = append(s.runtimes, r)
	return nil
}

func (s *SessionService) unregister(r *LocalRuntime) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, candidate := range s.runtimes {
		if candidate == r {
			s.runtimes = append(s.runtimes[:i], s.runtimes[i+1:]...)
			return
		}
	}
}

func (s *SessionService) claim(r *LocalRuntime, id string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return &SessionError{Kind: SessionErrorClosed, SessionID: id, Operation: SessionOperationCreateSession}
	}
	processSessions.Lock()
	defer processSessions.Unlock()
	if owner := processSessions.owners[id]; owner != nil {
		if owner == r {
			return nil
		}
		return &SessionError{Kind: SessionErrorConflict, SessionID: id, Operation: SessionOperationCreateSession, Detail: "session already has a process owner"}
	}
	count := 0
	for _, owner := range processSessions.owners {
		if owner.sessionService == s {
			count++
		}
	}
	if !limitAllows(count, s.maxSessions) {
		return &SessionError{Kind: SessionErrorCapacity, SessionID: id, Operation: SessionOperationCreateSession, Reason: SessionErrorReasonLimit, Limit: s.maxSessions}
	}
	processSessions.owners[id] = r
	return nil
}

func (s *SessionService) release(r *LocalRuntime, id string) {
	if s == nil {
		return
	}
	processSessions.Lock()
	defer processSessions.Unlock()
	if processSessions.owners[id] == r {
		delete(processSessions.owners, id)
	}
}

func (s *SessionService) owner(id string) (*LocalRuntime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, &SessionError{Kind: SessionErrorClosed, SessionID: id, Operation: SessionOperationLookup}
	}
	processSessions.Lock()
	defer processSessions.Unlock()
	r := processSessions.owners[id]
	if r == nil || r.sessionService != s {
		return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: id, Operation: SessionOperationLookup}
	}
	return r, nil
}

// Runtime returns a borrowed routing view, never shutdown authority.
func (s *SessionService) Runtime() SessionRuntime { return &serviceSessionRuntime{service: s} }

func (s *SessionService) SessionByID(id string) (SessionHandle, error) {
	r, err := s.owner(id)
	if err != nil {
		return nil, err
	}
	return r.SessionByID(id)
}

func (s *SessionService) runtimeForSession(ctx context.Context, id string) (*LocalRuntime, error) {
	if r, err := s.owner(id); err == nil {
		return r, nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, &SessionError{Kind: SessionErrorClosed, SessionID: id, Operation: SessionOperationLookup}
	}
	runtimes := append([]*LocalRuntime(nil), s.runtimes...)
	s.mu.Unlock()
	var selected *LocalRuntime
	for _, r := range runtimes {
		if r.sessionStore == nil {
			continue
		}
		stored, err := r.sessionStore.GetSession(ctx, id)
		if errors.Is(err, session.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		bound := stored.AttributesSnapshot()[SessionAgentAttribute]
		if bound == "" {
			continue
		}
		if _, err := r.team.Agent(bound); err != nil {
			continue
		}
		if stored.WorkingDir != "" && !sameWorkspace(stored.WorkingDir, r.workingDir) {
			continue
		}
		if selected != nil {
			return nil, &SessionError{Kind: SessionErrorConflict, SessionID: id, Operation: SessionOperationAttach, Detail: "multiple runtimes can load session; select its runtime explicitly"}
		}
		selected = r
	}
	if selected == nil {
		return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: id, Operation: SessionOperationLookup}
	}
	return selected, nil
}

func (s *SessionService) LoadSession(ctx context.Context, id string) (SessionHandle, *session.Session, error) {
	if handle, err := s.SessionByID(id); err == nil {
		snapshot, err := handle.Snapshot(ctx)
		return handle, snapshot, err
	}
	r, err := s.runtimeForSession(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	stored, err := r.sessionStore.GetSession(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if stored.ParentID != "" {
		prepared, err := (&localSessionRuntimeView{runtime: r}).PrepareSessionView(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		defer prepared.Abort()
		committed, err := prepared.Commit(ctx)
		if err != nil {
			return nil, nil, err
		}
		return committed.SessionHandle, committed.Info.Session, nil
	}
	return (&localSessionRuntimeView{runtime: r}).LoadSession(ctx, id)
}

func (s *SessionService) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	runtimes := append([]*LocalRuntime(nil), s.runtimes...)
	s.mu.Unlock()
	var result error
	for _, r := range runtimes {
		result = errors.Join(result, r.shutdownSessions(ctx))
	}
	return result
}

type serviceSessionRuntime struct{ service *SessionService }

func (v *serviceSessionRuntime) CreateSession(ctx context.Context, sess *session.Session, binding SessionBinding) (SessionHandle, error) {
	s := v.service
	if binding.ParentSessionID != "" {
		r, err := s.owner(binding.ParentSessionID)
		if err != nil {
			return nil, err
		}
		return r.CreateSession(ctx, sess, binding)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, &SessionError{Kind: SessionErrorClosed, Operation: SessionOperationCreateSession}
	}
	var selected *LocalRuntime
	for _, r := range s.runtimes {
		if binding.AgentName != "" {
			if _, err := r.team.Agent(binding.AgentName); err != nil {
				continue
			}
		}
		if selected != nil {
			s.mu.Unlock()
			return nil, &SessionError{Kind: SessionErrorConflict, Operation: SessionOperationCreateSession, Detail: "select an execution runtime for session creation"}
		}
		selected = r
	}
	s.mu.Unlock()
	if selected == nil {
		return nil, &SessionError{Kind: SessionErrorNotFound, Operation: SessionOperationCreateSession}
	}
	return selected.CreateSession(ctx, sess, binding)
}

func (v *serviceSessionRuntime) SessionByID(id string) (SessionHandle, error) {
	return v.service.SessionByID(id)
}

func (v *serviceSessionRuntime) DeleteSession(ctx context.Context, id string) error {
	r, err := v.service.runtimeForSession(ctx, id)
	if err != nil {
		return err
	}
	return r.DeleteSession(ctx, id)
}

func (v *serviceSessionRuntime) LoadSession(ctx context.Context, id string) (SessionHandle, *session.Session, error) {
	return v.service.LoadSession(ctx, id)
}

func (v *serviceSessionRuntime) PrepareSessionView(ctx context.Context, id string) (PreparedSessionView, error) {
	r, err := v.service.runtimeForSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return (&localSessionRuntimeView{runtime: r}).PrepareSessionView(ctx, id)
}

func (v *serviceSessionRuntime) ConfirmedSessionViewInfo(ctx context.Context, id string) (PreparedSessionViewInfo, error) {
	r, err := v.service.runtimeForSession(ctx, id)
	if err != nil {
		return PreparedSessionViewInfo{}, err
	}
	return (&localSessionRuntimeView{runtime: r}).ConfirmedSessionViewInfo(ctx, id)
}

var _ SessionRuntimeSupervisor = (*SessionService)(nil)
