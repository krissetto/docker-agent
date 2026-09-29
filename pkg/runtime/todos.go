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
	store   session.TodoStore
	scope   func(string) string
	changed func(string)
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
	key := s.scoped(sessionID)
	items, err := s.store.MutateTodos(ctx, key, fn)
	if err == nil && s.changed != nil {
		s.changed(key)
	}
	return items, err
}

func (s runtimeTodoStore) SaveTodos(ctx context.Context, sessionID string, items []session.Todo) error {
	key := s.scoped(sessionID)
	if err := s.store.SaveTodos(ctx, key, items); err != nil {
		return err
	}
	if s.changed != nil {
		s.changed(key)
	}
	return nil
}

// TodosChangedEvent invalidates a canonical todo list; consumers reload Todos.
type TodosChangedEvent struct {
	AgentContext
	Type          string `json:"type"`
	SessionID     string `json:"session_id"`
	TodoSessionID string `json:"todo_session_id"`
}

func (e *TodosChangedEvent) GetSessionID() string { return e.SessionID }

func (r *LocalRuntime) publishTodosChanged(key string) {
	r.sessionDrivers.mu.Lock()
	drivers := make(map[string]*sessionDriver, len(r.sessionDrivers.drivers))
	for id, driver := range r.sessionDrivers.drivers {
		drivers[id] = driver
	}
	r.sessionDrivers.mu.Unlock()
	for id, driver := range drivers {
		handle := &sessionHandle{runtime: r, driver: driver, sessionID: id}
		todoSet := handle.todoToolSet()
		if todoSet == nil {
			continue
		}
		target := id
		if todoSet.Shared() {
			target = r.todoRootSessionID(id)
		}
		if target == key {
			driver.events.Publish(id, &TodosChangedEvent{AgentContext: newAgentContext(""), Type: "todos_changed", SessionID: id, TodoSessionID: key})
		}
	}
}
