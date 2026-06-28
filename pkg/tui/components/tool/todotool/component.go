package todotool

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

const maxVisibleTodos = 10

// New creates a new unified todo component.
// This component handles create, create_multiple, list, and update operations.
func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return toolcommon.NewBaseWithCollapsed(msg, sessionState, render, toolcommon.NoArgsRenderer)
}

func render(msg *types.Message, s spinner.Spinner, sessionState service.SessionStateReader, width, _ int) string {
	header := toolcommon.NoArgsRenderer(msg, s, sessionState, width, 0)
	if sessionState.HideToolResults() {
		return header
	}

	details := renderDetails(msg, width)
	if details == "" {
		return header
	}
	return header + "\n" + styles.ToolCallResult.Render(details)
}

func renderDetails(msg *types.Message, width int) string {
	if msg.Content != "" {
		if details := renderOutputDetails(msg, width); details != "" {
			return details
		}
	}

	switch msg.ToolCall.Function.Name {
	case todo.ToolNameCreateTodo:
		args, err := toolcommon.ParseArgs[todo.CreateTodoArgs](msg.ToolCall.Function.Arguments)
		if err != nil || args.Description == "" {
			return ""
		}
		return formatTodoList([]todo.Todo{{Description: args.Description, Status: "pending"}}, width)
	case todo.ToolNameCreateTodos:
		args, err := toolcommon.ParseArgs[todo.CreateTodosArgs](msg.ToolCall.Function.Arguments)
		if err != nil || len(args.Descriptions) == 0 {
			return ""
		}
		todos := make([]todo.Todo, 0, len(args.Descriptions))
		for _, description := range args.Descriptions {
			todos = append(todos, todo.Todo{Description: description, Status: "pending"})
		}
		return formatTodoList(todos, width)
	case todo.ToolNameUpdateTodos:
		args, err := toolcommon.ParseArgs[todo.UpdateTodosArgs](msg.ToolCall.Function.Arguments)
		if err != nil || len(args.Updates) == 0 {
			return ""
		}
		return formatUpdates(args.Updates, width)
	}
	return ""
}

func renderOutputDetails(msg *types.Message, width int) string {
	switch msg.ToolCall.Function.Name {
	case todo.ToolNameCreateTodo:
		var out todo.CreateTodoOutput
		if err := json.Unmarshal([]byte(msg.Content), &out); err == nil && out.Created.Description != "" {
			return formatTodoList([]todo.Todo{out.Created}, width)
		}
	case todo.ToolNameCreateTodos:
		var out todo.CreateTodosOutput
		if err := json.Unmarshal([]byte(msg.Content), &out); err == nil && len(out.Created) > 0 {
			return formatTodoList(out.Created, width)
		}
	case todo.ToolNameUpdateTodos:
		var out todo.UpdateTodosOutput
		if err := json.Unmarshal([]byte(msg.Content), &out); err == nil && len(out.AllTodos) > 0 {
			return formatTodoList(out.AllTodos, width)
		}
	case todo.ToolNameListTodos:
		var out todo.ListTodosOutput
		if err := json.Unmarshal([]byte(msg.Content), &out); err == nil && len(out.Todos) > 0 {
			return formatTodoList(out.Todos, width)
		}
	}
	return ""
}

func formatTodoList(todos []todo.Todo, width int) string {
	if len(todos) == 0 {
		return ""
	}

	truncated := len(todos) > maxVisibleTodos
	if truncated {
		todos = todos[:maxVisibleTodos]
	}

	lines := make([]string, 0, len(todos)+1)
	for _, item := range todos {
		icon, _ := renderTodoIcon(item.Status)
		line := icon + " " + item.Description
		if item.ID != "" {
			line += " (" + item.ID + ")"
		}
		lines = append(lines, line)
	}
	if truncated {
		lines = append(lines, "…")
	}
	return wrapDetailLines(lines, width)
}

func formatUpdates(updates []todo.Update, width int) string {
	truncated := len(updates) > maxVisibleTodos
	if truncated {
		updates = updates[:maxVisibleTodos]
	}

	lines := make([]string, 0, len(updates)+1)
	for _, update := range updates {
		lines = append(lines, fmt.Sprintf("%s → %s", update.ID, update.Status))
	}
	if truncated {
		lines = append(lines, "…")
	}
	return wrapDetailLines(lines, width)
}

func wrapDetailLines(lines []string, width int) string {
	availableWidth := max(width-styles.ToolCallResult.GetHorizontalFrameSize(), 10)
	wrapped := make([]string, 0, len(lines))
	for _, line := range lines {
		wrapped = append(wrapped, toolcommon.WrapLines(line, availableWidth)...)
	}
	return strings.Join(wrapped, "\n")
}
