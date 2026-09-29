package todo

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

type SessionStore interface {
	LoadTodos(ctx context.Context, sessionID string) ([]Todo, error)
	SaveTodos(ctx context.Context, sessionID string, todos []Todo) error
	MutateTodos(ctx context.Context, sessionID string, fn func([]Todo) ([]Todo, error)) ([]Todo, error)
}

type SessionStorage struct {
	store    SessionStore
	resolve  func(context.Context) string
	mu       sync.Mutex
	fallback map[string][]Todo
}

func NewSessionStorage(store SessionStore, resolve func(context.Context) string) *SessionStorage {
	if resolve == nil {
		resolve = func(context.Context) string { return "" }
	}
	return &SessionStorage{store: store, resolve: resolve, fallback: map[string][]Todo{}}
}
func (s *SessionStorage) key(ctx context.Context) string { return s.resolve(ctx) }
func (s *SessionStorage) load(ctx context.Context) ([]Todo, error) {
	key := s.key(ctx)
	if key == "" {
		return append([]Todo(nil), s.fallback[key]...), nil
	}
	return s.store.LoadTodos(ctx, key)
}

func (s *SessionStorage) mutate(ctx context.Context, fn func([]Todo) ([]Todo, error)) error {
	key := s.key(ctx)
	if key != "" {
		_, err := s.store.MutateTodos(ctx, key, fn)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := fn(append([]Todo(nil), s.fallback[key]...))
	if err == nil {
		s.fallback[key] = append([]Todo(nil), items...)
	}
	return err
}

func (s *SessionStorage) AddTodo(ctx context.Context, description string) (Todo, error) {
	var created Todo
	err := s.mutate(ctx, func(items []Todo) ([]Todo, error) {
		// Removed IDs must never identify a later item selected by a stale view.
		created = Todo{ID: "todo_" + uuid.New().String(), Description: description, Status: "pending"}
		return append(items, created), nil
	})
	return created, err
}

func (s *SessionStorage) Add(ctx context.Context, item Todo) error {
	return s.mutate(ctx, func(items []Todo) ([]Todo, error) { return append(items, item), nil })
}

func (s *SessionStorage) AllE(ctx context.Context) ([]Todo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(ctx)
}

func (s *SessionStorage) UpdateByID(ctx context.Context, id string, fn func(Todo) Todo) (found bool, err error) {
	err = s.mutate(ctx, func(items []Todo) ([]Todo, error) {
		for i := range items {
			if items[i].ID == id {
				items[i] = fn(items[i])
				found = true
				break
			}
		}
		return items, nil
	})
	return found, err
}

func (s *SessionStorage) Clear(ctx context.Context) error {
	return s.mutate(ctx, func([]Todo) ([]Todo, error) { return []Todo{}, nil })
}

var _ Storage = (*SessionStorage)(nil)
