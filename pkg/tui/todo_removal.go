package tui

import (
	"context"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type todoRemovalConfirmedMsg struct {
	data                                 *panelSessionData
	scope                                messages.TodoScope
	item                                 session.Todo
	handle                               runtime.SessionHandle
	revision, selection, routeGeneration uint64
}

func (m *appModel) openTodoRemoval(msg messages.EditTodoMsg) tea.Cmd {
	data := m.panelOwnerData(msg.Scope.Owner)
	if m.contextClosed || data == nil || msg.Scope.Owner != m.paneFocus() || todoScope(msg.Scope.Owner, data) != msg.Scope {
		return notification.InfoCmd("The session changed; the todo was not removed")
	}
	if data.todoRemoval != nil && m.dialogMgr.HasDialog(func(d dialog.Dialog) bool { return d == data.todoRemoval }) {
		return nil
	}
	index := slices.IndexFunc(data.todos, func(item session.Todo) bool { return item.ID == msg.ID })
	if index < 0 {
		return notification.InfoCmd("Todo no longer exists")
	}
	handle := data.application.SessionHandle()
	if handle == nil || handle.ID() != msg.Scope.SessionID {
		return notification.ErrorCmd("Todo editing is unavailable")
	}
	request := todoRemovalConfirmedMsg{data: data, scope: msg.Scope, item: data.todos[index], handle: handle, revision: data.revision, selection: m.panelSelectionGeneration, routeGeneration: m.pendingRemovalGeneration}
	d := dialog.NewTodoRemovalDialog(core.CmdHandler(request))
	data.todoRemoval = d
	return core.CmdHandler(dialog.OpenDialogMsg{Model: d})
}

func (m *appModel) confirmTodoRemoval(msg todoRemovalConfirmedMsg) tea.Cmd {
	data := m.panelOwnerData(msg.scope.Owner)
	if m.contextClosed || data == nil || data != msg.data || todoScope(msg.scope.Owner, data) != msg.scope || msg.scope.Owner != m.paneFocus() || msg.selection != m.panelSelectionGeneration || msg.routeGeneration != m.pendingRemovalGeneration || data.application.SessionHandle() != msg.handle {
		return notification.InfoCmd("The session changed; the todo was not removed")
	}
	if data.revision != msg.revision || !slices.Contains(data.todos, msg.item) {
		return notification.InfoCmd("Todos changed; review the todo before removing it")
	}
	result := todoMutationMsg{data: data, scope: msg.scope, revision: data.revision}
	ctx := m.ctx()
	// Keep the captured canonical owner and opaque ID. RemoveTodo retains the
	// runtime's owner/store lock; description editing keeps its existing CAS.
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		result.todos, result.err = msg.handle.RemoveTodo(ctx, msg.item.ID)
		result.todos = slices.Clone(result.todos)
		return result
	}
}
