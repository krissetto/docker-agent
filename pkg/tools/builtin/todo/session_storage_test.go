package todo

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type atomicTestStore struct {
	mu    sync.Mutex
	items []Todo
}

func (s *atomicTestStore) LoadTodos(context.Context, string) ([]Todo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Todo(nil), s.items...), nil
}

func (s *atomicTestStore) SaveTodos(_ context.Context, _ string, items []Todo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]Todo(nil), items...)
	return nil
}

func (s *atomicTestStore) MutateTodos(_ context.Context, _ string, fn func([]Todo) ([]Todo, error)) ([]Todo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := fn(append([]Todo(nil), s.items...))
	if err == nil {
		s.items = append([]Todo(nil), items...)
	}
	return items, err
}

func TestSessionStorageConcurrentCreateIsAtomicAcrossInstances(t *testing.T) {
	store := &atomicTestStore{}
	a := New(WithStorage(NewSessionStorage(store, func(context.Context) string { return "root" })))
	b := New(WithStorage(NewSessionStorage(store, func(context.Context) string { return "root" })))
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		<-start
		_, err := a.handler.createTodo(t.Context(), CreateTodoArgs{Description: "a"})
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := b.handler.createTodo(t.Context(), CreateTodoArgs{Description: "b"})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	items, err := store.LoadTodos(t.Context(), "root")
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.NotEqual(t, items[0].ID, items[1].ID)
}
