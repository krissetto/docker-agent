package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/httpclient"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	todotool "github.com/docker/docker-agent/pkg/tools/builtin/todo"
)

func newTodoHandleFixture(t *testing.T, store session.Store, shared bool) (*LocalRuntime, SessionHandle) {
	t.Helper()
	a := agent.New("root", "prompt", agent.WithModel(&mockProvider{id: "test/mock-model"}), agent.WithToolSets(todotool.New(todotool.WithShared(shared))))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithSessionStore(store))
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	h, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	return r, h
}

func TestSessionHandleTodoMutationsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "todos.db")
	store, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	_, handle := newTodoHandleFixture(t, store, true)
	todos := store.(session.TodoStore)
	initial := []session.Todo{{ID: "todo_1", Description: "first", Status: "pending"}, {ID: "todo_2", Description: "second", Status: "pending"}}
	require.NoError(t, todos.SaveTodos(t.Context(), handle.ID(), initial))
	observation, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	for _, status := range []string{"in-progress", "completed", "pending"} {
		items, err := handle.SetTodoStatus(t.Context(), "todo_1", status)
		require.NoError(t, err)
		require.Len(t, items, 2)
		require.Equal(t, session.Todo{ID: "todo_1", Description: "first", Status: status}, items[0])
		requireTodoInvalidation(t, observation, handle.ID(), handle.ID())
	}
	items, err := handle.RemoveTodo(t.Context(), "todo_1")
	require.NoError(t, err)
	require.Equal(t, initial[1:], items)
	requireTodoInvalidation(t, observation, handle.ID(), handle.ID())
	// A separate connection reads committed storage, not a handle's projection.
	reopened, err := sqlitestore.New(t.Context(), path)
	require.NoError(t, err)
	defer reopened.Close()
	persisted, err := reopened.(session.TodoStore).LoadTodos(t.Context(), handle.ID())
	require.NoError(t, err)
	require.Equal(t, items, persisted)
	items, err = handle.RemoveTodo(t.Context(), "todo_2")
	require.NoError(t, err)
	require.Empty(t, items)
	persisted, err = reopened.(session.TodoStore).LoadTodos(t.Context(), handle.ID())
	require.NoError(t, err)
	require.Empty(t, persisted)
}

func requireTodoInvalidation(t *testing.T, observation Observation, sessionID, key string) {
	t.Helper()
	select {
	case event := <-observation.Events:
		changed, ok := event.Event.(*TodosChangedEvent)
		require.True(t, ok, "expected todo invalidation, got %T", event.Event)
		require.Equal(t, sessionID, event.SessionID)
		require.Equal(t, sessionID, changed.SessionID)
		require.Equal(t, key, changed.TodoSessionID)
	case <-time.After(5 * time.Second):
		t.Fatal("missing todo invalidation")
	}
}

func TestSessionHandleTodoRootSharingAndAgentInvalidation(t *testing.T) {
	for _, shared := range []bool{true, false} {
		t.Run(map[bool]string{true: "shared", false: "local"}[shared], func(t *testing.T) {
			store := session.NewInMemorySessionStore()
			r, root := newTodoHandleFixture(t, store, shared)
			child, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			other, err := r.CreateSession(t.Context(), session.New(), SessionBinding{AgentName: "root"})
			require.NoError(t, err)
			r.subagents.mu.Lock()
			r.subagents.sessions[root.ID()] = &sessionSubagents{topLevel: true, sess: root.(*sessionHandle).driver.session(), node: subagent.SessionRootID(root.ID())}
			r.subagents.sessions[child.ID()] = &sessionSubagents{node: "child"}
			r.subagents.children["child"] = &childRecord{sessionID: child.ID(), parentSession: root.ID()}
			r.subagents.mu.Unlock()
			key := child.ID()
			if shared {
				key = root.ID()
			}
			require.NoError(t, store.(session.TodoStore).SaveTodos(t.Context(), key, []session.Todo{{ID: "todo_1", Status: "pending"}}))
			observations := make(map[string]Observation)
			for _, h := range []SessionHandle{root, child, other} {
				obs, err := h.Observe(t.Context(), ObserveOptions{})
				require.NoError(t, err)
				defer obs.Cancel()
				observations[h.ID()] = obs
			}
			items, err := child.SetTodoStatus(t.Context(), "todo_1", "completed")
			require.NoError(t, err)
			require.Equal(t, "completed", items[0].Status)
			requireTodoInvalidation(t, observations[child.ID()], child.ID(), key)
			if shared {
				requireTodoInvalidation(t, observations[root.ID()], root.ID(), key)
				got, err := root.Todos(t.Context())
				require.NoError(t, err)
				require.Equal(t, items, got)
			}
			// The runtime-bound agent toolset emits the same invalidation.
			toolset := child.(*sessionHandle).todoToolSet()
			allTools, err := toolset.Tools(t.Context())
			require.NoError(t, err)
			for _, tool := range allTools {
				if tool.Name == todotool.ToolNameCreateTodo {
					_, err := tool.Handler(httpclient.ContextWithSessionID(t.Context(), child.ID()), tools.ToolCall{Function: tools.FunctionCall{Arguments: `{"description":"agent created"}`}}, tools.NopRuntime{})
					require.NoError(t, err)
				}
			}
			requireTodoInvalidation(t, observations[child.ID()], child.ID(), key)
			if shared {
				requireTodoInvalidation(t, observations[root.ID()], root.ID(), key)
			} else {
				require.Empty(t, root.(*sessionHandle).driver.events.replay[root.ID()])
			}
			items, err = child.RemoveTodo(t.Context(), "todo_1")
			require.NoError(t, err)
			require.Len(t, items, 1)
			requireTodoInvalidation(t, observations[child.ID()], child.ID(), key)
			if shared {
				requireTodoInvalidation(t, observations[root.ID()], root.ID(), key)
				got, err := root.Todos(t.Context())
				require.NoError(t, err)
				require.Equal(t, items, got)
			}
			require.Empty(t, other.(*sessionHandle).driver.events.replay[other.ID()])
		})
	}
}

type failingTodoStore struct {
	session.Store
	session.TodoStore
	err error
}

func (s *failingTodoStore) MutateTodos(context.Context, string, func([]session.Todo) ([]session.Todo, error)) ([]session.Todo, error) {
	return []session.Todo{{ID: "misleading-result", Status: "completed"}}, s.err
}

func TestSessionHandleTodoMutationErrors(t *testing.T) {
	store := session.NewInMemorySessionStore()
	r, handle := newTodoHandleFixture(t, store, false)
	initial := []session.Todo{{ID: "todo_1", Status: "pending"}}
	require.NoError(t, store.(session.TodoStore).SaveTodos(t.Context(), handle.ID(), initial))
	for _, tc := range []struct {
		name string
		call func() ([]session.Todo, error)
		kind SessionErrorKind
	}{
		{"invalid status", func() ([]session.Todo, error) { return handle.SetTodoStatus(t.Context(), "todo_1", "done") }, SessionErrorInvalid},
		{"blank status id", func() ([]session.Todo, error) { return handle.SetTodoStatus(t.Context(), "", "completed") }, SessionErrorInvalid},
		{"blank remove id", func() ([]session.Todo, error) { return handle.RemoveTodo(t.Context(), " ") }, SessionErrorInvalid},
		{"missing status id", func() ([]session.Todo, error) { return handle.SetTodoStatus(t.Context(), "missing", "completed") }, SessionErrorNotFound},
		{"missing remove id", func() ([]session.Todo, error) { return handle.RemoveTodo(t.Context(), "missing") }, SessionErrorNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items, err := tc.call()
			require.Nil(t, items)
			require.ErrorIs(t, err, &SessionError{Kind: tc.kind})
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := handle.RemoveTodo(ctx, "todo_1")
	require.ErrorIs(t, err, context.Canceled)
	_, err = handle.SetTodoStatus(ctx, "todo_1", "completed")
	require.ErrorIs(t, err, context.Canceled)
	failure := errors.New("todo commit failed")
	r.sessionStore = &failingTodoStore{Store: store, TodoStore: store.(session.TodoStore), err: failure}
	items, err := handle.SetTodoStatus(t.Context(), "todo_1", "completed")
	require.ErrorIs(t, err, failure)
	require.Nil(t, items)
	items, err = handle.RemoveTodo(t.Context(), "todo_1")
	require.ErrorIs(t, err, failure)
	require.Nil(t, items)
	r.sessionStore = store
	persisted, err := store.(session.TodoStore).LoadTodos(t.Context(), handle.ID())
	require.NoError(t, err)
	require.Equal(t, initial, persisted)
	d := handle.(*sessionHandle).driver
	require.Empty(t, d.events.replay[handle.ID()])
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	_, err = handle.RemoveTodo(t.Context(), "todo_1")
	require.ErrorIs(t, err, ErrSessionStopped)
	d.mu.Lock()
	d.stopped = false
	d.mu.Unlock()
}

func TestSessionHandleTodoConcurrentAgentWritesAndRemoval(t *testing.T) {
	store := session.NewInMemorySessionStore()
	_, handle := newTodoHandleFixture(t, store, false)
	storage := todotool.NewSessionStorage(store.(session.TodoStore), func(context.Context) string { return handle.ID() })
	original, err := storage.AddTodo(t.Context(), "same description")
	require.NoError(t, err)
	_, err = handle.RemoveTodo(t.Context(), original.ID)
	require.NoError(t, err)
	replacement, err := storage.AddTodo(t.Context(), original.Description)
	require.NoError(t, err)
	require.NotEqual(t, original.ID, replacement.ID)
	_, err = handle.RemoveTodo(t.Context(), original.ID)
	require.ErrorIs(t, err, &SessionError{Kind: SessionErrorNotFound})
	_, err = handle.SetTodoStatus(t.Context(), original.ID, "completed")
	require.ErrorIs(t, err, &SessionError{Kind: SessionErrorNotFound})
	removed, err := storage.AddTodo(t.Context(), "remove concurrently")
	require.NoError(t, err)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 52)
	for range 50 {
		wg.Go(func() {
			<-start
			_, err := storage.AddTodo(t.Context(), "concurrent creation")
			errs <- err
		})
	}
	wg.Go(func() {
		<-start
		_, err := handle.SetTodoStatus(t.Context(), replacement.ID, "completed")
		errs <- err
	})
	wg.Go(func() {
		<-start
		_, err := handle.RemoveTodo(t.Context(), removed.ID)
		errs <- err
	})
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	items, err := handle.Todos(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 51)
	ids := make(map[string]bool)
	for _, item := range items {
		require.False(t, ids[item.ID])
		ids[item.ID] = true
		require.NotEqual(t, removed.ID, item.ID)
		if item.ID == replacement.ID {
			require.Equal(t, "completed", item.Status)
		}
	}
}

func TestSessionTransportDecodesTodosChangedEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher := writeCanonicalObservationStart(w)
		fmt.Fprintf(w, `data: {"version":%d,"type":"event","envelope":{"version":%d,"session_id":"s","sequence":1,"event":{"type":"todos_changed","session_id":"s","todo_session_id":"root"}}}`+"\n\n", api.SessionAPIVersion, api.SessionAPIVersion)
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	client, err := NewClient(srv.URL, WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	requireTodoInvalidation(t, observation, "s", "root")
	_, err = handle.SetTodoStatus(t.Context(), "todo_1", "completed")
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = handle.RemoveTodo(t.Context(), "todo_1")
	require.ErrorIs(t, err, ErrUnsupported)
}

func TestSessionHandleTodoMutationsWithoutToolsetUnsupported(t *testing.T) {
	r, sess := newSessionFixture(t)
	handle, err := r.CreateSession(t.Context(), sess, SessionBinding{})
	require.NoError(t, err)
	_, err = handle.SetTodoStatus(t.Context(), "todo_1", "completed")
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = handle.RemoveTodo(t.Context(), "todo_1")
	require.ErrorIs(t, err, ErrUnsupported)
}
