package session

import "context"

// Todo is a session-scoped progress item persisted independently from the
// transcript. It intentionally mirrors the todo tool's stable wire shape.
type Todo struct {
	ID          string `json:"id" jsonschema:"ID of the todo item"`
	Description string `json:"description" jsonschema:"Description of the todo item"`
	Status      string `json:"status" jsonschema:"Status of the todo item (pending, in-progress, completed)"`
}

// TodoStore is the optional capability implemented by session stores that can
// persist todo snapshots. Implementations must isolate values by session ID.
type TodoStore interface {
	LoadTodos(ctx context.Context, sessionID string) ([]Todo, error)
	SaveTodos(ctx context.Context, sessionID string, todos []Todo) error
	MutateTodos(ctx context.Context, sessionID string, fn func([]Todo) ([]Todo, error)) ([]Todo, error)
}
