package todotool

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tools"
	todo "github.com/docker/docker-agent/pkg/tools/builtin/todo/types"
	"github.com/docker/docker-agent/pkg/tui/components/tab"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// renderCacheCap bounds the per-width render cache. Only a couple of widths are
// ever live at once (the two-pass scrollbar layout renders at two widths), so a
// small cap soaks up a window resize without letting the cache grow unbounded
// between todo updates.
const renderCacheCap = 8

// SidebarComponent represents the todo display component for the sidebar
type SidebarComponent struct {
	todos []todo.Todo
	width int

	// renderCache memoizes the rendered todo list keyed by content width.
	// Render() is called on every animation frame while the agent works (the
	// sidebar rebuilds its whole cache on each spinner tick) and the two-pass
	// scrollbar layout renders at two widths, so without this the O(todos)
	// word-wrap+style work runs many times per second on long lists. Cleared
	// when the todos change (SetTodos) or the theme changes (InvalidateCache).
	renderCache map[int]string
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
	c.renderCache = nil // todos changed — drop the stale render cache
	return nil
}

// InvalidateCache drops the memoized render. The sidebar calls this when the
// theme changes, since the cached lines embed theme-dependent styling.
func (c *SidebarComponent) InvalidateCache() {
	c.renderCache = nil
}

// Counts returns how many todos are completed and the total count.
func (c *SidebarComponent) Counts() (completed, total int) {
	for _, t := range c.todos {
		if t.Status == "completed" {
			completed++
		}
	}
	return completed, len(c.todos)
}

func (c *SidebarComponent) Render() string {
	if len(c.todos) == 0 {
		return ""
	}

	if cached, ok := c.renderCache[c.width]; ok {
		return cached
	}

	var lines []string
	for _, todo := range c.todos {
		lines = append(lines, c.renderTodoLine(todo))
	}
	rendered := c.renderTab("TO-DO", strings.Join(lines, "\n"))

	if c.renderCache == nil || len(c.renderCache) >= renderCacheCap {
		c.renderCache = make(map[int]string, 2)
	}
	c.renderCache[c.width] = rendered
	return rendered
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
