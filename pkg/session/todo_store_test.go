package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInMemorySessionStoreTodosAreSessionKeyed(t *testing.T) {
	store := NewInMemorySessionStore()
	a, b := New(), New()
	require.NoError(t, store.AddSession(t.Context(), a))
	require.NoError(t, store.AddSession(t.Context(), b))
	todos := store.(TodoStore)
	require.NoError(t, todos.SaveTodos(t.Context(), a.ID, []Todo{{ID: "todo_1"}}))
	gotA, err := todos.LoadTodos(t.Context(), a.ID)
	require.NoError(t, err)
	require.Len(t, gotA, 1)
	gotB, err := todos.LoadTodos(t.Context(), b.ID)
	require.NoError(t, err)
	require.Empty(t, gotB)
	require.NoError(t, store.DeleteSession(t.Context(), a.ID))
	_, err = todos.LoadTodos(t.Context(), a.ID)
	require.ErrorIs(t, err, ErrNotFound)
}
