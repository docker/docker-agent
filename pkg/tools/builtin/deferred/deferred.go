package deferred

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameSearchTool = "search_tool"
	ToolNameAddTool    = "add_tool"
)

type deferredToolEntry struct {
	tool   tools.Tool
	source tools.ToolSet
}

type Toolset struct {
	mu             sync.RWMutex
	deferredTools  map[string]deferredToolEntry
	activatedTools map[string]tools.Tool
	sources        []deferredSource
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*Toolset)(nil)
	_ tools.Startable    = (*Toolset)(nil)
	_ tools.Instructable = (*Toolset)(nil)
)

type deferredSource struct {
	toolset  tools.ToolSet
	deferAll bool
	tools    []string
}

func NewDeferredToolset() *Toolset {
	return &Toolset{
		deferredTools:  make(map[string]deferredToolEntry),
		activatedTools: make(map[string]tools.Tool),
	}
}

func (d *Toolset) AddSource(toolset tools.ToolSet, deferAll bool, toolNames []string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.sources = append(d.sources, deferredSource{
		toolset:  toolset,
		deferAll: deferAll,
		tools:    toolNames,
	})
}

func (d *Toolset) HasSources() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.sources) > 0
}

func (d *Toolset) Instructions() string {
	return `## Deferred Tools

Use search_tool to discover additional tools by keyword (e.g., "remote", "read", "write"). Use add_tool to activate a discovered tool.`
}

type SearchToolArgs struct {
	Query string `json:"query" jsonschema:"Search query to find tools by name or description (case-insensitive)"`
}

type SearchToolResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type AddToolArgs struct {
	Name string `json:"name" jsonschema:"The name of the tool to activate"`
}

func (d *Toolset) handleSearchTool(_ context.Context, args SearchToolArgs) (*tools.ToolCallResult, error) {
	query := strings.ToLower(args.Query)

	d.mu.RLock()
	defer d.mu.RUnlock()

	var results []SearchToolResult
	for name, entry := range d.deferredTools {
		// Search in name and description
		// TODO: fuzzy search? Levenshtein distance? Semantic search?
		if strings.Contains(strings.ToLower(name), query) ||
			strings.Contains(strings.ToLower(entry.tool.Description), query) {
			results = append(results, SearchToolResult{
				Name:        name,
				Description: entry.tool.Description,
			})
		}
	}

	if len(results) == 0 {
		return tools.ResultError(fmt.Sprintf("No deferred tools found matching '%s'", args.Query)), nil
	}

	output, err := json.Marshal(results)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal results: %w", err)
	}

	return tools.ResultSuccess(fmt.Sprintf("Found %d deferred tool(s):\n%s", len(results), string(output))), nil
}

func (d *Toolset) handleAddTool(_ context.Context, args AddToolArgs) (*tools.ToolCallResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, exists := d.activatedTools[args.Name]; exists {
		return tools.ResultSuccess(fmt.Sprintf("Tool '%s' is already active", args.Name)), nil
	}

	entry, exists := d.deferredTools[args.Name]
	if !exists {
		return tools.ResultError(fmt.Sprintf("Tool '%s' not found.", args.Name)), nil
	}

	delete(d.deferredTools, args.Name)
	d.activatedTools[args.Name] = entry.tool

	return tools.ResultSuccess(fmt.Sprintf("Tool '%s' has been activated and is now available for use.\n\nDescription: %s", args.Name, entry.tool.Description)), nil
}

func (d *Toolset) Tools(context.Context) ([]tools.Tool, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	result := []tools.Tool{
		{
			Name:         ToolNameSearchTool,
			Category:     "deferred",
			Description:  "Search for available deferred tools by name or description. Use this to discover tools that can be activated.",
			Parameters:   tools.MustSchemaFor[SearchToolArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(d.handleSearchTool),
			Annotations: tools.ToolAnnotations{
				Title:        "Search Tool",
				ReadOnlyHint: true,
			},
		},
		{
			Name:         ToolNameAddTool,
			Category:     "deferred",
			Description:  "Activate a deferred tool by name, making it available for use. Use search_tool first to find available tools.",
			Parameters:   tools.MustSchemaFor[AddToolArgs](),
			OutputSchema: tools.MustSchemaFor[string](),
			Handler:      tools.NewHandler(d.handleAddTool),
			Annotations: tools.ToolAnnotations{
				Title:        "Add Tool",
				ReadOnlyHint: true,
			},
		},
	}

	for _, tool := range d.activatedTools {
		result = append(result, tool)
	}

	return result, nil
}

func (d *Toolset) Start(ctx context.Context) error {
	// Note: we are not responsible for starting the underlying toolsets here
	d.mu.RLock()
	defer d.mu.RUnlock()

	for _, source := range d.sources {
		allTools, err := source.toolset.Tools(ctx)
		if err != nil {
			return fmt.Errorf("failed to get tools from source: %w", err)
		}

		for _, tool := range allTools {
			if !source.deferAll && !slices.Contains(source.tools, tool.Name) {
				continue
			}

			if _, exists := d.deferredTools[tool.Name]; !exists {
				d.deferredTools[tool.Name] = deferredToolEntry{
					tool:   tool,
					source: source.toolset,
				}
			}
		}
	}

	return nil
}

func (d *Toolset) Stop(context.Context) error {
	return nil
}
