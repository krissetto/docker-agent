package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// annotateTodoSpan stamps the operation kind, batch size, and the
// resulting list size onto the active runtime.tool.handler span so a
// glance at a session shows when the agent was actually managing
// progress vs. just chatting.
func annotateTodoSpan(ctx context.Context, op string, batch, total, completed int) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(
		attribute.String("cagent.tool.todo.op", op),
		attribute.Int("cagent.tool.todo.batch_size", batch),
		attribute.Int("cagent.tool.todo.total", total),
		attribute.Int("cagent.tool.todo.completed", completed),
	)
}

// countCompleted returns how many todos in the current snapshot are
// marked completed. Cheap O(n) scan over a typically-tiny slice; called
// once per todo handler invocation for the span annotation.
func countCompleted(all []Todo) int {
	n := 0
	for _, t := range all {
		if t.Status == "completed" {
			n++
		}
	}
	return n
}

const (
	ToolNameCreateTodo  = "create_todo"
	ToolNameCreateTodos = "create_todos"
	ToolNameUpdateTodos = "update_todos"
	ToolNameListTodos   = "list_todos"
)

// CreateToolSet is used by the tools registry.
func CreateToolSet(toolset latest.Toolset) (tools.ToolSet, error) {
	return New(WithShared(toolset.Shared)), nil
}

type ToolSet struct {
	handler *todoHandler
	shared  bool
}

// Todo is the durable session-scoped todo record. This alias preserves the
// builtin package's public API and stable JSON schema while sharing one DTO.
type Todo = session.Todo

type CreateTodoArgs struct {
	Description string `json:"description" jsonschema:"Description of the todo item"`
}

type CreateTodosArgs struct {
	Descriptions []string `json:"descriptions" jsonschema:"Descriptions of the todo items"`
}

type Update struct {
	ID     string `json:"id" jsonschema:"ID of the todo item"`
	Status string `json:"status" jsonschema:"New status (pending, in-progress, completed)"`
}

type UpdateTodosArgs struct {
	Updates []Update `json:"updates" jsonschema:"List of todo updates"`
}

// Output types for JSON-structured responses.

type CreateTodoOutput struct {
	Created  Todo   `json:"created" jsonschema:"The created todo item"`
	AllTodos []Todo `json:"all_todos" jsonschema:"Current state of all todo items"`
	Reminder string `json:"reminder,omitempty" jsonschema:"Reminder about incomplete todos that still need to be completed"`
}

type CreateTodosOutput struct {
	Created  []Todo `json:"created" jsonschema:"List of created todo items"`
	AllTodos []Todo `json:"all_todos" jsonschema:"Current state of all todo items"`
	Reminder string `json:"reminder,omitempty" jsonschema:"Reminder about incomplete todos that still need to be completed"`
}

type UpdateTodosOutput struct {
	Updated  []Update `json:"updated,omitempty" jsonschema:"List of successfully updated todos"`
	NotFound []string `json:"not_found,omitempty" jsonschema:"IDs of todos that were not found"`
	AllTodos []Todo   `json:"all_todos" jsonschema:"Current state of all todo items"`
	Reminder string   `json:"reminder,omitempty" jsonschema:"Reminder about incomplete todos that still need to be completed"`
}

type ListTodosOutput struct {
	Todos    []Todo `json:"todos" jsonschema:"List of all current todo items"`
	Reminder string `json:"reminder,omitempty" jsonschema:"Reminder about incomplete todos that still need to be completed"`
}

// Storage defines the storage layer for todo items.
type Storage interface {
	Add(ctx context.Context, todo Todo) error
	AllE(ctx context.Context) ([]Todo, error)
	UpdateByID(ctx context.Context, id string, fn func(Todo) Todo) (bool, error)
	Clear(ctx context.Context) error
}

// MemoryTodoStorage is an in-memory, concurrency-safe implementation of Storage.
type MemoryTodoStorage struct {
	todos *concurrent.Slice[Todo]
}

func NewMemoryTodoStorage() *MemoryTodoStorage {
	return &MemoryTodoStorage{
		todos: concurrent.NewSlice[Todo](),
	}
}

func (s *MemoryTodoStorage) Add(_ context.Context, todo Todo) error {
	s.todos.Append(todo)
	return nil
}

func (s *MemoryTodoStorage) All(_ context.Context) []Todo {
	return s.todos.All()
}
func (s *MemoryTodoStorage) AllE(ctx context.Context) ([]Todo, error) { return s.All(ctx), nil }
func (s *MemoryTodoStorage) Len(_ context.Context) int                { return s.todos.Length() }

func (s *MemoryTodoStorage) UpdateByID(_ context.Context, id string, fn func(Todo) Todo) (bool, error) {
	_, idx := s.todos.Find(func(t Todo) bool { return t.ID == id })
	if idx < 0 {
		return false, nil
	}
	s.todos.Update(idx, fn)
	return true, nil
}

func (s *MemoryTodoStorage) Clear(_ context.Context) error {
	s.todos.Clear()
	return nil
}

// Option is a functional option for configuring a Tool.
type Option func(*ToolSet)

// WithStorage sets a custom storage implementation for the Tool.
// The provided storage must not be nil.
func WithStorage(storage Storage) Option {
	if storage == nil {
		panic("todo: storage must not be nil")
	}
	return func(t *ToolSet) {
		t.handler.storage = storage
	}
}

func WithShared(shared bool) Option {
	return func(t *ToolSet) { t.shared = shared }
}

func (t *ToolSet) Shared() bool { return t.shared }

type todoHandler struct {
	storage Storage
	nextID  atomic.Int64
}

func New(opts ...Option) *ToolSet {
	t := &ToolSet{
		handler: &todoHandler{
			storage: NewMemoryTodoStorage(),
		},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *ToolSet) SetStorage(storage Storage) {
	if storage == nil {
		return
	}
	t.handler.storage = storage
}

// Todos returns the current session-keyed snapshot. The runtime supplies the
// session ID on ctx.
func (t *ToolSet) Todos(ctx context.Context) ([]Todo, error) {
	return t.handler.storage.AllE(ctx)
}

func (t *ToolSet) Instructions() string {
	return `## Todo Tools

Track task progress with todos:
- Create todos for each major step before starting complex work (prefer batch create_todos)
- Update status to "in-progress" before starting; mark "completed" only when the work is actually finished
- Ending a turn to wait, ask a blocking question, or answer an unrelated request does not mean the task is done
- Keep unfinished todos pending or in-progress while waiting or blocked
- Batch multiple updates in a single update_todos call`
}

// addTodo creates a new todo and adds it to storage.
func (h *todoHandler) addTodo(ctx context.Context, description string) (Todo, error) {
	if storage, ok := h.storage.(interface {
		AddTodo(ctx context.Context, description string) (Todo, error)
	}); ok {
		return storage.AddTodo(ctx, description)
	}
	all, err := h.storage.AllE(ctx)
	if err != nil {
		return Todo{}, err
	}
	// Reconcile the allocator with durable state on every process/toolset start.
	for _, existing := range all {
		if n, err := strconv.ParseInt(strings.TrimPrefix(existing.ID, "todo_"), 10, 64); err == nil && existing.ID == fmt.Sprintf("todo_%d", n) {
			for current := h.nextID.Load(); n > current && !h.nextID.CompareAndSwap(current, n); current = h.nextID.Load() {
			}
		}
	}
	todo := Todo{
		ID:          fmt.Sprintf("todo_%d", h.nextID.Add(1)),
		Description: description,
		Status:      "pending",
	}
	if err := h.storage.Add(ctx, todo); err != nil {
		return Todo{}, err
	}
	return todo, nil
}

// jsonResult builds a ToolCallResult with a JSON-serialized output and current storage as Meta.
func (h *todoHandler) jsonResult(ctx context.Context, v any) (*tools.ToolCallResult, error) {
	all, err := h.storage.AllE(ctx)
	if err != nil {
		return nil, fmt.Errorf("todo storage: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshaling todo output: %w", err)
	}
	return &tools.ToolCallResult{
		Output: string(out),
		Meta:   all,
	}, nil
}

func (h *todoHandler) createTodo(ctx context.Context, params CreateTodoArgs) (*tools.ToolCallResult, error) {
	created, err := h.addTodo(ctx, params.Description)
	if err != nil {
		return nil, err
	}
	all, err := h.storage.AllE(ctx)
	if err != nil {
		return nil, err
	}
	annotateTodoSpan(ctx, "create_todo", 1, len(all), countCompleted(all))
	return h.jsonResult(ctx, CreateTodoOutput{
		Created:  created,
		AllTodos: all,
		Reminder: h.incompleteReminder(ctx),
	})
}

func (h *todoHandler) createTodos(ctx context.Context, params CreateTodosArgs) (*tools.ToolCallResult, error) {
	created := make([]Todo, 0, len(params.Descriptions))
	for _, desc := range params.Descriptions {
		item, err := h.addTodo(ctx, desc)
		if err != nil {
			return nil, err
		}
		created = append(created, item)
	}
	all, err := h.storage.AllE(ctx)
	if err != nil {
		return nil, err
	}
	annotateTodoSpan(ctx, "create_todos", len(params.Descriptions), len(all), countCompleted(all))
	return h.jsonResult(ctx, CreateTodosOutput{
		Created:  created,
		AllTodos: all,
		Reminder: h.incompleteReminder(ctx),
	})
}

func (h *todoHandler) updateTodos(ctx context.Context, params UpdateTodosArgs) (*tools.ToolCallResult, error) {
	result := UpdateTodosOutput{}

	for _, update := range params.Updates {
		found, err := h.storage.UpdateByID(ctx, update.ID, func(t Todo) Todo {
			t.Status = update.Status
			return t
		})
		if err != nil {
			return nil, err
		}
		if !found {
			result.NotFound = append(result.NotFound, update.ID)
			continue
		}
		result.Updated = append(result.Updated, update)
	}

	if len(result.NotFound) > 0 && len(result.Updated) == 0 {
		res, err := h.jsonResult(ctx, result)
		if err != nil {
			return nil, err
		}
		res.IsError = true
		return res, nil
	}

	allTodos, err := h.storage.AllE(ctx)
	result.AllTodos = allTodos
	if err != nil {
		return nil, err
	}
	result.Reminder = h.incompleteReminder(ctx)
	annotateTodoSpan(ctx, "update_todos", len(params.Updates), len(result.AllTodos), countCompleted(result.AllTodos))

	return h.jsonResult(ctx, result)
}

// incompleteReminder returns a reminder string listing any non-completed todos,
// or an empty string if all are completed (or storage is empty).
func (h *todoHandler) incompleteReminder(ctx context.Context) string {
	all, err := h.storage.AllE(ctx)
	if err != nil {
		return ""
	}
	var pending, inProgress []string
	for _, todo := range all {
		switch todo.Status {
		case "pending":
			pending = append(pending, fmt.Sprintf("[%s] %s", todo.ID, todo.Description))
		case "in-progress":
			inProgress = append(inProgress, fmt.Sprintf("[%s] %s", todo.ID, todo.Description))
		}
	}
	if len(pending) == 0 && len(inProgress) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("The following todos are still incomplete:")
	for _, s := range inProgress {
		b.WriteString(" (in-progress) " + s)
	}
	for _, s := range pending {
		b.WriteString(" (pending) " + s)
	}
	return b.String()
}

func (h *todoHandler) listTodos(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
	todos, err := h.storage.AllE(ctx)
	if err != nil {
		return nil, err
	}
	if todos == nil {
		todos = []Todo{}
	}
	annotateTodoSpan(ctx, "list_todos", 0, len(todos), countCompleted(todos))
	out := ListTodosOutput{Todos: todos}
	out.Reminder = h.incompleteReminder(ctx)
	return h.jsonResult(ctx, out)
}

func (t *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{
			Name:         ToolNameCreateTodo,
			Category:     "todo",
			Description:  "Create a new todo item with a description",
			Parameters:   tools.MustSchemaFor[CreateTodoArgs](),
			OutputSchema: tools.MustSchemaFor[CreateTodoOutput](),
			Handler:      tools.NewHandler(t.handler.createTodo),
			Annotations: tools.ToolAnnotations{
				Title:        "Create TODO",
				ReadOnlyHint: true, // Technically not read-only but has practically no destructive side effects.
			},
		},
		{
			Name:         ToolNameCreateTodos,
			Category:     "todo",
			Description:  "Create a list of new todo items with descriptions",
			Parameters:   tools.MustSchemaFor[CreateTodosArgs](),
			OutputSchema: tools.MustSchemaFor[CreateTodosOutput](),
			Handler:      tools.NewHandler(t.handler.createTodos),
			Annotations: tools.ToolAnnotations{
				Title:        "Create TODOs",
				ReadOnlyHint: true, // Technically not read-only but has practically no destructive side effects.
			},
		},
		{
			Name:         ToolNameUpdateTodos,
			Category:     "todo",
			Description:  "Update the status of one or more todo item(s)",
			Parameters:   tools.MustSchemaFor[UpdateTodosArgs](),
			OutputSchema: tools.MustSchemaFor[UpdateTodosOutput](),
			Handler:      tools.NewHandler(t.handler.updateTodos),
			Annotations: tools.ToolAnnotations{
				Title:        "Update TODOs",
				ReadOnlyHint: true, // Technically not read-only but has practically no destructive side effects.
			},
		},
		{
			Name:         ToolNameListTodos,
			Category:     "todo",
			Description:  "List all current todos with their status",
			OutputSchema: tools.MustSchemaFor[ListTodosOutput](),
			Handler:      t.handler.listTodos,
			Annotations: tools.ToolAnnotations{
				Title:        "List TODOs",
				ReadOnlyHint: true,
			},
		},
	}, nil
}
