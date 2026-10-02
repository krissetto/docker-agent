package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

func todoRemovalRoot(t *testing.T) (*appModel, *editableTodoHandle) {
	t.Helper()
	sess := session.New(session.WithID("todo-removal-owner"))
	store := session.NewInMemorySessionStore().(*session.InMemorySessionStore)
	require.NoError(t, store.AddSession(t.Context(), sess))
	require.NoError(t, store.SaveTodos(t.Context(), sess.ID, []session.Todo{
		{ID: "first", Description: "Duplicate todo", Status: "pending"},
		{ID: "second", Description: "Duplicate todo", Status: "pending"},
	}))
	handle := &editableTodoHandle{panelTodoHandle: &panelTodoHandle{lifecycleHandle: &lifecycleHandle{id: sess.ID}, capable: true}, store: store}
	application := app.New(t.Context(), &editableTodoRuntime{handle: handle}, sess, runtime.SessionBinding{}, app.WithRuntimeServices(stubRuntime{}))
	root := newSidebarProgramRoot(t, application)
	for _, data := range root.panelData {
		data.attempted, data.requested = false, false
	}
	for _, msg := range collectMsgs(root.prepareTodos()) {
		if loaded, ok := msg.(panelTodosMsg); ok {
			root.acceptPanelTodos(loaded)
		}
	}
	chat.CancelSidebarPresentation(root.chatPage)
	return root, handle
}

func openTodoRemovalConfirmation(t *testing.T, root *appModel, scope messages.TodoScope, id string) todoRemovalConfirmedMsg {
	t.Helper()
	cmd := root.editTodo(messages.EditTodoMsg{Scope: scope, ID: id, Remove: true})
	require.NotNil(t, cmd)
	opened := cmd().(dialog.OpenDialogMsg)
	root.updateDialogCmd(opened)
	_, cmd = opened.Model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	var request todoRemovalConfirmedMsg
	for _, msg := range collectMsgs(cmd) {
		if confirmed, ok := msg.(todoRemovalConfirmedMsg); ok {
			request = confirmed
		} else {
			root.updateDialogCmd(msg)
		}
	}
	require.NotEmpty(t, request.item.ID)
	return request
}

func TestTodoRemovalRejectsStaleConfirmation(t *testing.T) {
	for _, change := range []string{"revision", "description", "status", "deletion", "reset", "route-round-trip", "pane-round-trip", "handle"} {
		t.Run(change, func(t *testing.T) {
			root, handle := todoRemovalRoot(t)
			owner := root.paneFocus()
			data := root.panelData[owner]
			request := openTodoRemovalConfirmation(t, root, todoScope(owner, data), "second")
			switch change {
			case "revision":
				data.revision++
			case "description":
				data.todos[1].Description = "Changed"
			case "status":
				data.todos[1].Status = "completed"
			case "deletion":
				data.todos = data.todos[:1]
			case "reset":
				root.ingestPanelEvent(owner, &app.SessionResetEvent{})
			case "route-round-trip":
				root.pendingRemovalGeneration++
			case "pane-round-trip":
				root.panelSelectionGeneration++
			case "handle":
				root.application.NewSession()
			}
			result := root.confirmTodoRemoval(request)()
			require.IsType(t, notification.ShowMsg{}, result)
			stored, err := handle.Todos(t.Context())
			require.NoError(t, err)
			require.Len(t, stored, 2)
		})
	}
}

func TestTodoRemovalPinsIDOwnerAndReportsMissingItem(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		root, handle := todoRemovalRoot(t)
		owner := root.paneFocus()
		request := openTodoRemovalConfirmation(t, root, todoScope(owner, root.panelData[owner]), "second")
		cmd := root.confirmTodoRemoval(request)
		if deleted {
			_, err := handle.RemoveTodo(t.Context(), "second")
			require.NoError(t, err)
		}
		root.application.NewSession() // Worker may not follow the new App route.
		result := cmd().(todoMutationMsg)
		if deleted {
			require.ErrorContains(t, result.err, "missing todo")
		} else {
			require.NoError(t, result.err)
		}
		stored, err := handle.Todos(t.Context())
		require.NoError(t, err)
		require.Equal(t, []session.Todo{{ID: "first", Description: "Duplicate todo", Status: "pending"}}, stored)
	}
}

func TestTodoRemovalActualRouteRoundTripInvalidatesConfirmation(t *testing.T) {
	root := splitTestRoot(t)
	data := root.panelOwnerData("profile")
	data.todos = []session.Todo{{ID: "todo", Description: "Task", Status: "pending"}}
	request := todoRemovalConfirmedMsg{data: data, scope: todoScope("profile", data), item: data.todos[0], handle: root.application.SessionHandle(), revision: data.revision, selection: root.panelSelectionGeneration, routeGeneration: root.pendingRemovalGeneration}
	root.handleSwitchTab("second")
	root.handleSwitchTab("profile")
	require.Contains(t, root.confirmTodoRemoval(request)().(notification.ShowMsg).Text, "session changed")
}

func TestTodoRemovalFailureIsPublishedWithoutOptimisticDeletion(t *testing.T) {
	root, handle := todoRemovalRoot(t)
	owner := root.paneFocus()
	data := root.panelData[owner]
	request := openTodoRemovalConfirmation(t, root, todoScope(owner, data), "second")
	_, err := handle.RemoveTodo(t.Context(), "second")
	require.NoError(t, err)
	result := root.confirmTodoRemoval(request)().(todoMutationMsg)
	require.ErrorContains(t, result.err, "missing todo")
	found := false
	for _, msg := range collectMsgs(root.finishTodoMutation(result)) {
		if notice, ok := msg.(notification.ShowMsg); ok {
			found = true
			require.Equal(t, notification.TypeError, notice.Type)
			require.Contains(t, notice.Text, "missing todo")
		}
	}
	require.True(t, found)
	require.Len(t, data.todos, 2, "failed mutation does not optimistically remove a row")
}

func TestTodoRemovalDisplaysSelectedSnapshot(t *testing.T) {
	root, _ := todoRemovalRoot(t)
	owner := root.paneFocus()
	data := root.panelData[owner]
	data.todos[1].Description = "Chosen todo λ界\nsecond line"
	item := data.todos[1]
	cmd := root.editTodo(messages.EditTodoMsg{Scope: todoScope(owner, data), ID: item.ID, Remove: true})
	require.NotNil(t, cmd)
	opened := cmd().(dialog.OpenDialogMsg)
	data.todos[1].Description = "Changed later"
	opened.Model.SetSize(120, 40)
	view := ansi.Strip(opened.Model.View())
	require.Contains(t, view, "Chosen todo λ界")
	require.Contains(t, view, "second line")
	require.NotContains(t, view, "Duplicate todo")
	require.NotContains(t, view, "Changed later")
	_, cmd = opened.Model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	var request todoRemovalConfirmedMsg
	for _, msg := range collectMsgs(cmd) {
		if confirmed, ok := msg.(todoRemovalConfirmedMsg); ok {
			request = confirmed
		}
	}
	require.Equal(t, item, request.item, "display and confirmation use the same captured todo")
	require.Contains(t, root.confirmTodoRemoval(request)().(notification.ShowMsg).Text, "Todos changed")
}
