package todotool

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/todo"
	"github.com/docker/docker-agent/pkg/tui/components/tab"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// SidebarComponent represents the todo display component for the sidebar
type SidebarComponent struct {
	todos []todo.Todo
	width int
}

func NewSidebarComponent() *SidebarComponent {
	return &SidebarComponent{
		width: 20,
	}
}

func (c *SidebarComponent) SetSize(width int) {
	c.width = width
}

func (c *SidebarComponent) SetTodos(result *tools.ToolCallResult) error {
	if result == nil || result.Meta == nil {
		return nil
	}

	todos, ok := result.Meta.([]todo.Todo)
	if !ok {
		return nil
	}

	c.todos = todos
	return nil
}

func (c *SidebarComponent) Render() string {
	if len(c.todos) == 0 {
		return ""
	}

	var lines []string
	for _, todo := range c.todos {
		lines = append(lines, c.renderTodoLine(todo))
	}

	return c.renderTab("TO-DO", strings.Join(lines, "\n"))
}

func (c *SidebarComponent) renderTodoLine(todoItem todo.Todo) string {
	icon, style := renderTodoIcon(todoItem.Status)

	prefix := icon + " "
	prefixWidth := lipgloss.Width(prefix)
	maxDescWidth := max(1, c.width-prefixWidth)

	wrapped := toolcommon.WrapLinesWords(todoItem.Description, maxDescWidth)
	indent := strings.Repeat(" ", prefixWidth)

	var b strings.Builder
	for i, line := range wrapped {
		if i == 0 {
			b.WriteString(prefix + line)
		} else {
			b.WriteString("\n" + indent + line)
		}
	}

	return styles.TabPrimaryStyle.Render(style.Render(b.String()))
}

func (c *SidebarComponent) renderTab(title, content string) string {
	return tab.Render(title, content, c.width)
}
