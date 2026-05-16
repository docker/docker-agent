package todo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/concurrent"
	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameCreateTodo  = "create_todo"
	ToolNameCreateTodos = "create_todos"
	ToolNameUpdateTodos = "update_todos"
	ToolNameListTodos   = "list_todos"
)

type Tool struct {
	handler *todoHandler
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*Tool)(nil)
	_ tools.Instructable = (*Tool)(nil)
)

type Todo struct {
	ID          string `json:"id" jsonschema:"ID of the todo item"`
	Description string `json:"description" jsonschema:"Description of the todo item"`
	Status      string `json:"status" jsonschema:"Status of the todo item (pending, in-progress, completed)"`
}

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
	// Add appends a new todo item.
	Add(ctx context.Context, todo Todo)
	// All returns a copy of all todo items.
	All(ctx context.Context) []Todo
	// Len returns the number of todo items.
	Len(ctx context.Context) int
	// FindByID returns the index of the todo with the given ID, or -1 if not found.
	FindByID(ctx context.Context, id string) int
	// Update modifies the todo at the given index using the provided function.
	Update(ctx context.Context, index int, fn func(Todo) Todo)
	// Clear removes all todo items.
	Clear(ctx context.Context)
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

func (s *MemoryTodoStorage) Add(_ context.Context, todo Todo) {
	s.todos.Append(todo)
}

func (s *MemoryTodoStorage) All(_ context.Context) []Todo {
	return s.todos.All()
}

func (s *MemoryTodoStorage) Len(_ context.Context) int {
	return s.todos.Length()
}

func (s *MemoryTodoStorage) FindByID(_ context.Context, id string) int {
	_, idx := s.todos.Find(func(t Todo) bool { return t.ID == id })
	return idx
}

func (s *MemoryTodoStorage) Update(_ context.Context, index int, fn func(Todo) Todo) {
	s.todos.Update(index, fn)
}

func (s *MemoryTodoStorage) Clear(_ context.Context) {
	s.todos.Clear()
}

// Option is a functional option for configuring a Tool.
type Option func(*Tool)

// WithStorage sets a custom storage implementation for the Tool.
// The provided storage must not be nil.
func WithStorage(storage Storage) Option {
	if storage == nil {
		panic("todo: storage must not be nil")
	}
	return func(t *Tool) {
		t.handler.storage = storage
	}
}

type todoHandler struct {
	storage Storage
	nextID  atomic.Int64
}

var NewSharedTodoTool = sync.OnceValue(func() *Tool { return NewTodoTool() })

func NewTodoTool(opts ...Option) *Tool {
	t := &Tool{
		handler: &todoHandler{
			storage: NewMemoryTodoStorage(),
		},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (t *Tool) Instructions() string {
	return `## Todo Tools

Track task progress with todos:
- Create todos for each major step before starting complex work (prefer batch create_todos)
- Update status to "in-progress" before starting, "completed" immediately after finishing
- Every todo MUST be marked "completed" before your final response
- Batch multiple updates in a single update_todos call
- Never leave todos pending or in-progress when done`
}

// addTodo creates a new todo and adds it to storage.
func (h *todoHandler) addTodo(ctx context.Context, description string) Todo {
	todo := Todo{
		ID:          fmt.Sprintf("todo_%d", h.nextID.Add(1)),
		Description: description,
		Status:      "pending",
	}
	h.storage.Add(ctx, todo)
	return todo
}

// jsonResult builds a ToolCallResult with a JSON-serialized output and current storage as Meta.
func (h *todoHandler) jsonResult(ctx context.Context, v any) (*tools.ToolCallResult, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshaling todo output: %w", err)
	}
	return &tools.ToolCallResult{
		Output: string(out),
		Meta:   h.storage.All(ctx),
	}, nil
}

func (h *todoHandler) createTodo(ctx context.Context, params CreateTodoArgs) (*tools.ToolCallResult, error) {
	created := h.addTodo(ctx, params.Description)
	return h.jsonResult(ctx, CreateTodoOutput{
		Created:  created,
		AllTodos: h.storage.All(ctx),
		Reminder: h.incompleteReminder(ctx),
	})
}

func (h *todoHandler) createTodos(ctx context.Context, params CreateTodosArgs) (*tools.ToolCallResult, error) {
	created := make([]Todo, 0, len(params.Descriptions))
	for _, desc := range params.Descriptions {
		created = append(created, h.addTodo(ctx, desc))
	}
	return h.jsonResult(ctx, CreateTodosOutput{
		Created:  created,
		AllTodos: h.storage.All(ctx),
		Reminder: h.incompleteReminder(ctx),
	})
}

func (h *todoHandler) updateTodos(ctx context.Context, params UpdateTodosArgs) (*tools.ToolCallResult, error) {
	result := UpdateTodosOutput{}

	for _, update := range params.Updates {
		idx := h.storage.FindByID(ctx, update.ID)
		if idx == -1 {
			result.NotFound = append(result.NotFound, update.ID)
			continue
		}

		h.storage.Update(ctx, idx, func(t Todo) Todo {
			t.Status = update.Status
			return t
		})
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

	result.AllTodos = h.storage.All(ctx)
	result.Reminder = h.incompleteReminder(ctx)

	return h.jsonResult(ctx, result)
}

// incompleteReminder returns a reminder string listing any non-completed todos,
// or an empty string if all are completed (or storage is empty).
func (h *todoHandler) incompleteReminder(ctx context.Context) string {
	all := h.storage.All(ctx)
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
	b.WriteString("The following todos are still incomplete and MUST be completed:")
	for _, s := range inProgress {
		b.WriteString(" (in-progress) " + s)
	}
	for _, s := range pending {
		b.WriteString(" (pending) " + s)
	}
	return b.String()
}

func (h *todoHandler) listTodos(ctx context.Context, _ tools.ToolCall) (*tools.ToolCallResult, error) {
	todos := h.storage.All(ctx)
	if todos == nil {
		todos = []Todo{}
	}
	out := ListTodosOutput{Todos: todos}
	out.Reminder = h.incompleteReminder(ctx)
	return h.jsonResult(ctx, out)
}

func (t *Tool) Tools(context.Context) ([]tools.Tool, error) {
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
