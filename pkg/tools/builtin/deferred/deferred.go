package deferred

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

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

type ToolSet struct {
	mu             sync.RWMutex
	deferredTools  map[string]deferredToolEntry
	activatedTools map[string]tools.Tool
	sources        []deferredSource
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Startable    = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
)

type deferredSource struct {
	toolset  tools.ToolSet
	deferAll bool
	tools    []string
}

func New() *ToolSet {
	return &ToolSet{
		deferredTools:  make(map[string]deferredToolEntry),
		activatedTools: make(map[string]tools.Tool),
	}
}

func (d *ToolSet) AddSource(toolset tools.ToolSet, deferAll bool, toolNames []string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.sources = append(d.sources, deferredSource{
		toolset:  toolset,
		deferAll: deferAll,
		tools:    toolNames,
	})
}

func (d *ToolSet) HasSources() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.sources) > 0
}

func (d *ToolSet) Instructions() string {
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

func (d *ToolSet) handleSearchTool(ctx context.Context, args SearchToolArgs) (*tools.ToolCallResult, error) {
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

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(
			attribute.String("cagent.tool.deferred.op", "search_tool"),
			attribute.String("cagent.tool.deferred.query", args.Query),
			attribute.Int("cagent.tool.deferred.match_count", len(results)),
			attribute.Int("cagent.tool.deferred.pool_size", len(d.deferredTools)),
		)
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

func (d *ToolSet) handleAddTool(ctx context.Context, args AddToolArgs) (*tools.ToolCallResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	span := trace.SpanFromContext(ctx)
	annotate := func(outcome string) {
		if !span.IsRecording() {
			return
		}
		span.SetAttributes(
			attribute.String("cagent.tool.deferred.op", "add_tool"),
			attribute.String("cagent.tool.deferred.tool_name", args.Name),
			attribute.String("cagent.tool.deferred.outcome", outcome),
			attribute.Int("cagent.tool.deferred.activated_count", len(d.activatedTools)),
		)
	}

	if _, exists := d.activatedTools[args.Name]; exists {
		annotate("already_active")
		return tools.ResultSuccess(fmt.Sprintf("Tool '%s' is already active", args.Name)), nil
	}

	entry, exists := d.deferredTools[args.Name]
	if !exists {
		annotate("not_found")
		return tools.ResultError(fmt.Sprintf("Tool '%s' not found.", args.Name)), nil
	}

	delete(d.deferredTools, args.Name)
	d.activatedTools[args.Name] = entry.tool
	annotate("activated")

	return tools.ResultSuccess(fmt.Sprintf("Tool '%s' has been activated and is now available for use.\n\nDescription: %s", args.Name, entry.tool.Description)), nil
}

func (d *ToolSet) Tools(context.Context) ([]tools.Tool, error) {
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

func (d *ToolSet) Start(ctx context.Context) error {
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

func (d *ToolSet) Stop(context.Context) error {
	return nil
}
