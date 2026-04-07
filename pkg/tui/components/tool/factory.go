package tool

import (
	"github.com/docker/docker-agent/pkg/tools/builtin"
	"github.com/docker/docker-agent/pkg/tui/components/tool/api"
	"github.com/docker/docker-agent/pkg/tui/components/tool/defaulttool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/directorytree"
	"github.com/docker/docker-agent/pkg/tui/components/tool/editfile"
	"github.com/docker/docker-agent/pkg/tui/components/tool/globfiles"
	"github.com/docker/docker-agent/pkg/tui/components/tool/handoff"
	"github.com/docker/docker-agent/pkg/tui/components/tool/listdirectory"
	"github.com/docker/docker-agent/pkg/tui/components/tool/readfile"
	"github.com/docker/docker-agent/pkg/tui/components/tool/readmultiplefiles"
	"github.com/docker/docker-agent/pkg/tui/components/tool/searchfilescontent"
	"github.com/docker/docker-agent/pkg/tui/components/tool/shell"
	"github.com/docker/docker-agent/pkg/tui/components/tool/todotool"
	"github.com/docker/docker-agent/pkg/tui/components/tool/transfertask"
	"github.com/docker/docker-agent/pkg/tui/components/tool/writefile"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Factory creates tool components using the registry.
// It looks up registered component builders and falls back to a default component
// if no specific builder is registered for a tool.
type Factory struct {
	registry *Registry
}

func NewFactory(registry *Registry) *Factory {
	return &Factory{
		registry: registry,
	}
}

func (f *Factory) Create(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	toolName := msg.ToolCall.Function.Name

	// First try to match by exact tool name
	if builder, ok := f.registry.Get(toolName); ok {
		return builder(msg, sessionState)
	}

	// Then try to match by category
	if msg.ToolDefinition.Category != "" {
		if builder, ok := f.registry.Get("category:" + msg.ToolDefinition.Category); ok {
			return builder(msg, sessionState)
		}
	}

	return defaulttool.New(msg, sessionState)
}

var (
	defaultRegistry = newDefaultRegistry()
	defaultFactory  = NewFactory(defaultRegistry)
)

func newDefaultRegistry() *Registry {
	registry := NewRegistry()

	// Define tool registrations declaratively.
	// Tools with the same visual representation share a builder.
	registry.RegisterAll([]Registration{
		{[]string{builtin.ToolNameTransferTask}, transfertask.New},
		{[]string{builtin.ToolNameHandoff}, handoff.New},
		{[]string{builtin.ToolNameEditFile}, editfile.New},
		{[]string{builtin.ToolNameWriteFile}, writefile.New},
		{[]string{builtin.ToolNameReadFile}, readfile.New},
		{[]string{builtin.ToolNameReadMultipleFiles}, readmultiplefiles.New},
		{[]string{builtin.ToolNameListDirectory}, listdirectory.New},
		{[]string{builtin.ToolNameDirectoryTree}, directorytree.New},
		{[]string{builtin.ToolNameSearchFilesContent}, searchfilescontent.New},
		{[]string{builtin.ToolNameGlobFiles}, globfiles.New},
		{[]string{builtin.ToolNameShell}, shell.New},
		{[]string{builtin.ToolNameFetch, "category:api"}, api.New},
		{
			[]string{
				builtin.ToolNameCreateTodo,
				builtin.ToolNameCreateTodos,
				builtin.ToolNameUpdateTodos,
				builtin.ToolNameListTodos,
			},
			todotool.New,
		},
	})

	return registry
}

func New(msg *types.Message, sessionState service.SessionStateReader) layout.Model {
	return defaultFactory.Create(msg, sessionState)
}
