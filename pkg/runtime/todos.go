package runtime

import (
	"context"

	"github.com/docker/docker-agent/pkg/session"
)

func (r *LocalRuntime) todoRootSessionID(sessionID string) string {
	if r.subagents == nil {
		return sessionID
	}
	r.subagents.mu.Lock()
	defer r.subagents.mu.Unlock()
	root := r.subagents.rootSessionLocked(sessionID)
	if root == "" {
		return sessionID
	}
	return root
}

type runtimeTodoStore struct {
	store session.TodoStore
	scope func(string) string
}

func (s runtimeTodoStore) scoped(sessionID string) string {
	if s.scope != nil {
		return s.scope(sessionID)
	}
	return sessionID
}

func (s runtimeTodoStore) LoadTodos(ctx context.Context, sessionID string) ([]session.Todo, error) {
	return s.store.LoadTodos(ctx, s.scoped(sessionID))
}

func (s runtimeTodoStore) MutateTodos(ctx context.Context, sessionID string, fn func([]session.Todo) ([]session.Todo, error)) ([]session.Todo, error) {
	return s.store.MutateTodos(ctx, s.scoped(sessionID), fn)
}

func (s runtimeTodoStore) SaveTodos(ctx context.Context, sessionID string, items []session.Todo) error {
	return s.store.SaveTodos(ctx, s.scoped(sessionID), items)
}
