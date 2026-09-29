package tui

import (
	"context"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func todoScope(owner string, data *panelSessionData) messages.TodoScope {
	return messages.TodoScope{Owner: owner, SessionID: data.sessionID, Generation: data.generation, Epoch: data.epoch}
}
func (m *appModel) prepareTodos() tea.Cmd {
	if m.application == nil || m.supervisor == nil || m.contextClosed {
		return nil
	}
	owner := m.paneFocus()
	if m.panelSelectionOwner != owner {
		m.panelSelectionOwner = owner
		m.panelSelectionGeneration++
	}
	m.panelOwnerData(owner)
	var cmds []tea.Cmd
	for owner, data := range m.panelData {
		if data.todosKnown && data.attempted && !data.requested && data.publishedRevision != data.revision {
			cmds = append(cmds, m.publishTodos(owner, data, nil))
		}
		if data.attempted {
			continue
		}
		runner := m.supervisor.GetRunner(owner)
		if runner == nil || runner.App != data.application {
			continue
		}
		handle := runner.App.SessionHandle()
		data.attempted = true
		if handle == nil || !handle.Metadata().Capabilities.Todos {
			continue
		}
		data.requested = true
		result := panelTodosMsg{owner: owner, sessionID: data.sessionID, generation: data.generation, revision: data.revision}
		if owner == m.paneFocus() {
			result.selection = m.panelSelectionGeneration
		}
		ctx := m.ctx()
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			todos, err := handle.Todos(ctx)
			result.todos, result.err = slices.Clone(todos), err
			return result
		})
	}
	return tea.Batch(cmds...)
}
func (m *appModel) publishTodos(owner string, data *panelSessionData, err error) tea.Cmd {
	data.publishedRevision = data.revision
	snapshot := messages.TodosSnapshotMsg{Scope: todoScope(owner, data), Todos: slices.Clone(data.todos), Err: err}
	var cmds []tea.Cmd
	if page := m.chatPages[owner]; page != nil {
		_, cmd := page.Update(snapshot)
		cmds = append(cmds, cmd)
	}
	cmds = append(cmds, m.updateDialogCmd(snapshot))
	return tea.Batch(cmds...)
}
func (m *appModel) openTodos(msg messages.OpenTodosMsg) tea.Cmd {
	owner := m.paneFocus()
	data := m.panelOwnerData(owner)
	if data == nil {
		return nil
	}
	if msg.Scope.SessionID != "" && msg.Scope != todoScope(owner, data) {
		return nil
	}
	d := dialog.NewTodosDialog(todoScope(owner, data), data.todos, msg.ID)
	data.todoDialog = d
	return core.CmdHandler(dialog.OpenDialogMsg{Model: d})
}

type todoMutationMsg struct {
	data     *panelSessionData
	scope    messages.TodoScope
	revision uint64
	todos    []session.Todo
	err      error
}

func (m *appModel) editTodo(msg messages.EditTodoMsg) tea.Cmd {
	data := m.panelOwnerData(msg.Scope.Owner)
	if data == nil || todoScope(msg.Scope.Owner, data) != msg.Scope || msg.Scope.Owner != m.paneFocus() {
		return nil
	}
	runner := m.supervisor.GetRunner(msg.Scope.Owner)
	if runner == nil || runner.App != data.application {
		return nil
	}
	handle := runner.App.SessionHandle()
	if handle == nil {
		return notification.ErrorCmd("Todo editing is unavailable")
	}
	result := todoMutationMsg{data: data, scope: msg.Scope, revision: data.revision}
	ctx := m.ctx()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if msg.Remove {
			result.todos, result.err = handle.RemoveTodo(ctx, msg.ID)
		} else {
			result.todos, result.err = handle.SetTodoStatus(ctx, msg.ID, msg.Status)
		}
		result.todos = slices.Clone(result.todos)
		return result
	}
}
func (m *appModel) finishTodoMutation(msg todoMutationMsg) tea.Cmd {
	data := m.panelData[msg.scope.Owner]
	if data == nil || data != msg.data || todoScope(msg.scope.Owner, data) != msg.scope {
		return nil
	}
	generation, ok := m.supervisor.RouteGeneration(msg.scope.Owner)
	if !ok || generation != msg.scope.Generation {
		return nil
	}
	if msg.err != nil {
		return tea.Batch(m.publishTodos(msg.scope.Owner, data, msg.err), notification.ErrorCmd("Failed to update todo: "+msg.err.Error()))
	}
	// An invalidation racing the result is newer truth: reload instead of replacing it.
	if data.revision != msg.revision {
		data.attempted = false
		return m.prepareTodos()
	}
	m.panelRevision++
	data.revision = m.panelRevision
	data.todos = slices.Clone(msg.todos)
	data.todosKnown = true
	data.requested = false
	data.attempted = true
	m.viewCacheValid = false
	return m.publishTodos(msg.scope.Owner, data, nil)
}

func (m *appModel) syncPanelTreeDialog() {
	if m.dialogMgr == nil || !m.dialogMgr.Open() || m.dialogMgr.Closing() {
		return
	}
	// Tree refresh carries no ownership state; only the current focused inspector receives it.
	top := m.dialogMgr.TopDialog()
	if _, ok := top.(interface{ SetCancel(func()) }); !ok {
		return
	}
	data := m.panelData[m.paneFocus()]
	if data == nil || data.treeDialog != top {
		return
	}
	m.updateDialogCmd(dialog.SubagentsRefreshMsg{Dialog: top, Nodes: data.nodes})
}
