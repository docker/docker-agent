// Package types defines the shared wire contracts for the todo tools.
package types

const (
	ToolNameCreateTodo  = "create_todo"
	ToolNameCreateTodos = "create_todos"
	ToolNameUpdateTodos = "update_todos"
	ToolNameListTodos   = "list_todos"
)

type Todo struct {
	ID          string `json:"id" jsonschema:"ID of the todo item"`
	Description string `json:"description" jsonschema:"Description of the todo item"`
	Status      string `json:"status" jsonschema:"Status of the todo item (pending, in-progress, completed)"`
}
