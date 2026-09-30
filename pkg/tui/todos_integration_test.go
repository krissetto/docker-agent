package tui

import (
	"context"
	"errors"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type editableTodoHandle struct {
	*panelTodoHandle
	store *session.InMemorySessionStore
}

func (h *editableTodoHandle) Todos(ctx context.Context) ([]session.Todo, error) {
	return h.store.LoadTodos(ctx, h.ID())
}

func (h *editableTodoHandle) SetTodoStatus(ctx context.Context, id, status string) ([]session.Todo, error) {
	return h.store.MutateTodos(ctx, h.ID(), func(items []session.Todo) ([]session.Todo, error) {
		for i := range items {
			if items[i].ID == id {
				items[i].Status = status
				return items, nil
			}
		}
		return nil, errors.New("missing todo")
	})
}

func (h *editableTodoHandle) RemoveTodo(ctx context.Context, id string) ([]session.Todo, error) {
	return h.store.MutateTodos(ctx, h.ID(), func(items []session.Todo) ([]session.Todo, error) {
		for i := range items {
			if items[i].ID == id {
				return append(items[:i], items[i+1:]...), nil
			}
		}
		return nil, errors.New("missing todo")
	})
}

type editableTodoRuntime struct {
	runtime.SessionRuntime
	handle *editableTodoHandle
}

func (r *editableTodoRuntime) SessionByID(string) (runtime.SessionHandle, error) {
	return r.handle, nil
}

func TestTodoMutationPersistsRefreshesDialogAndHiddenPanelAndFencesReset(t *testing.T) {
	root := panelFixture(t)
	sess := root.application.Session()
	store := session.NewInMemorySessionStore().(*session.InMemorySessionStore)
	require.NoError(t, store.AddSession(t.Context(), sess))
	require.NoError(t, store.SaveTodos(t.Context(), sess.ID, []session.Todo{{ID: "opaque", Description: "canonical row", Status: "pending"}}))
	handle := &editableTodoHandle{panelTodoHandle: &panelTodoHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, capable: true}, store: store}
	application := app.New(t.Context(), &editableTodoRuntime{handle: handle}, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(stubRuntime{}))
	root.application = application
	root.supervisor.GetRunner("profile").App = application
	root.panelSettings = messages.PanelSettings{Elements: []messages.PanelElement{}}
	for _, msg := range collectMsgs(root.preparePanel()) {
		if loaded, ok := msg.(panelTodosMsg); ok {
			root.acceptPanelTodos(loaded)
		}
	}
	data := root.panelData["profile"]
	require.True(t, data.todosKnown, "hidden footer still hydrates canonical todos")
	opened := root.openTodos(messages.OpenTodosMsg{})().(dialog.OpenDialogMsg)
	root.updateDialogCmd(opened)
	scope := todoScope("profile", data)
	result := root.editTodo(messages.EditTodoMsg{Scope: scope, ID: "opaque", Status: "completed"})().(todoMutationMsg)
	root.finishTodoMutation(result)
	require.Equal(t, "completed", data.todos[0].Status)
	require.Contains(t, ansi.Strip(root.dialogMgr.TopDialog().View()), "●")
	stored, err := store.LoadTodos(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "completed", stored[0].Status)
	require.False(t, root.chatPage.IsWorking(), "snapshot is not a synthetic tool event")
	// Agent invalidation hydrates even with footer disabled.
	require.NoError(t, store.SaveTodos(t.Context(), sess.ID, []session.Todo{{ID: "opaque", Description: "canonical row", Status: "in-progress"}}))
	root.ingestPanelEvent("profile", &runtime.TodosChangedEvent{SessionID: sess.ID, TodoSessionID: sess.ID})
	root.ingestPanelEvent("profile", todoEvent([]session.Todo{{ID: "opaque", Status: "pending"}}))
	for _, msg := range collectMsgs(root.preparePanel()) {
		if loaded, ok := msg.(panelTodosMsg); ok {
			root.acceptPanelTodos(loaded)
		}
	}
	require.Equal(t, "in-progress", data.todos[0].Status)
	remove := root.editTodo(messages.EditTodoMsg{Scope: scope, ID: "opaque", Remove: true})().(todoMutationMsg)
	root.finishTodoMutation(remove)
	stored, err = store.LoadTodos(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, stored)
	require.Contains(t, ansi.Strip(root.dialogMgr.TopDialog().View()), "No todos")
	root.ingestPanelEvent("profile", &app.SessionResetEvent{})
	require.True(t, !root.dialogMgr.HasActiveDialog() || root.dialogMgr.Closing(), "reset retires the old scoped inspector before it can emit a stale action")
	root.finishTodoMutation(result)
	require.False(t, root.panelData["profile"].todosKnown, "completion cannot overwrite a reset even with identical session ID")
}

func (h *editableTodoHandle) SetTodoDescription(ctx context.Context, id, expected, description string) ([]session.Todo, error) {
	return h.store.MutateTodos(ctx, h.ID(), func(items []session.Todo) ([]session.Todo, error) {
		for i := range items {
			if items[i].ID == id {
				if items[i].Description != expected {
					return nil, errors.New("description conflict")
				}
				items[i].Description = description
				return items, nil
			}
		}
		return nil, errors.New("missing todo")
	})
}

func TestTodoEditorSaveRoutesAndFencesResetAndRevision(t *testing.T) {
	root := panelFixture(t)
	sess := root.application.Session()
	store := session.NewInMemorySessionStore().(*session.InMemorySessionStore)
	require.NoError(t, store.AddSession(t.Context(), sess))
	require.NoError(t, store.SaveTodos(t.Context(), sess.ID, []session.Todo{{ID: "opaque", Description: "original", Status: "pending"}}))
	handle := &editableTodoHandle{panelTodoHandle: &panelTodoHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, capable: true}, store: store}
	application := app.New(t.Context(), &editableTodoRuntime{handle: handle}, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(stubRuntime{}))
	root.application = application
	root.supervisor.GetRunner("profile").App = application
	for _, msg := range collectMsgs(root.preparePanel()) {
		if loaded, ok := msg.(panelTodosMsg); ok {
			root.acceptPanelTodos(loaded)
		}
	}
	data := root.panelData["profile"]
	scope := todoScope("profile", data)
	opened := root.openTodoEditor(messages.OpenTodoEditMsg{Scope: scope, ID: "opaque"})().(dialog.OpenDialogMsg)
	root.updateDialogCmd(opened)
	require.Same(t, opened.Model, data.todoEditor)
	request := messages.SaveTodoDescriptionMsg{Scope: scope, ID: "opaque", EditorID: 1, RequestID: 1, ExpectedDescription: "original", Description: "edited\nsecond line"}
	stale := request
	stale.Scope.Epoch++
	require.Nil(t, root.saveTodoDescription(stale))
	result := root.saveTodoDescription(request)().(todoDescriptionResult)
	root.panelRevision++
	data.revision = root.panelRevision
	reload := root.finishTodoDescription(result)
	require.Equal(t, "original", data.todos[0].Description, "racing revision must canonical reload, not replace")
	for _, msg := range collectMsgs(reload) {
		if loaded, ok := msg.(panelTodosMsg); ok {
			root.acceptPanelTodos(loaded)
		}
	}
	require.Equal(t, "edited\nsecond line", data.todos[0].Description, "canonical reload publishes the new revision")
	stored, err := store.LoadTodos(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, "edited\nsecond line", stored[0].Description)
	root.ingestPanelEvent("profile", &app.SessionResetEvent{})
	require.True(t, !root.dialogMgr.HasActiveDialog() || root.dialogMgr.Closing(), "reset closes scoped editor")
	require.Nil(t, root.finishTodoDescription(result), "stale completion cannot publish to reset session")
}
