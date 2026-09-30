package messages

import "github.com/docker/docker-agent/pkg/session"

// TodoScope fences actions to the canonical session and mounted route generation.
type TodoScope struct {
	Owner, SessionID string
	Generation       uint64
	Epoch            uint64
}
type OpenTodosMsg struct {
	ID    string
	Scope TodoScope
}
type EditTodoMsg struct {
	Scope      TodoScope
	ID, Status string
	Remove     bool
}
type TodosSnapshotMsg struct {
	Scope TodoScope
	Todos []session.Todo
	Err   error
}

func (TodosSnapshotMsg) BroadcastToDialogs() {}

type OpenTodoEditMsg struct {
	Scope TodoScope
	ID    string
}
type SaveTodoDescriptionMsg struct {
	Scope                            TodoScope
	ID                               string
	EditorID                         uint64
	RequestID                        uint64
	ExpectedDescription, Description string
}
type TodoSaveResultMsg struct {
	Scope     TodoScope
	ID        string
	EditorID  uint64
	RequestID uint64
	Todos     []session.Todo
	Err       error
}

func (TodoSaveResultMsg) BroadcastToDialogs() {}
