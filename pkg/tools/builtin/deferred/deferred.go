package deferred

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/junegunn/fzf/src/algo"
	"github.com/junegunn/fzf/src/util"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/docker/docker-agent/pkg/tools"
)

const (
	ToolNameSearchTool = "search_tool"
	ToolNameAddTool    = "add_tool"
)

// ToolSet exposes search_tool/add_tool over the deferred tools of its
// sources. Each source is snapshotted lazily, on the first search/add after
// it can be listed, rather than at start: the sources are the agent's other
// toolsets and may still be starting when the first turn runs, so a
// start-time snapshot would race them and fail the whole toolset.
type ToolSet struct {
	mu             sync.RWMutex
	activatedTools map[string]tools.Tool
	sources        []*deferredSource
}

// Verify interface compliance
var (
	_ tools.ToolSet      = (*ToolSet)(nil)
	_ tools.Instructable = (*ToolSet)(nil)
	_ tools.Named        = (*ToolSet)(nil)
)

type deferredSource struct {
	toolset  tools.ToolSet
	deferAll bool
	tools    []string

	// snapshot holds the source's deferred tools once listed successfully;
	// listed stays false while the source is unavailable so it is retried.
	snapshot []tools.Tool
	listed   bool
}

func (s *deferredSource) defers(name string) bool {
	return s.deferAll || slices.Contains(s.tools, name)
}

func New() *ToolSet {
	return &ToolSet{
		activatedTools: make(map[string]tools.Tool),
	}
}

// Name implements tools.Named; loader-created, so no registry WithName wrapper.
func (d *ToolSet) Name() string {
	return "deferred"
}

func (d *ToolSet) AddSource(toolset tools.ToolSet, deferAll bool, toolNames []string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.sources = append(d.sources, &deferredSource{
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

// snapshotPendingSources lists every source not yet snapshotted. Sources are
// listed without holding d.mu (an MCP list may be a network round-trip);
// a source that cannot list yet (typically still starting) is skipped and
// retried on the next call, so its tools show up once it is ready.
func (d *ToolSet) snapshotPendingSources(ctx context.Context) {
	d.mu.RLock()
	var pending []*deferredSource
	for _, source := range d.sources {
		if !source.listed {
			pending = append(pending, source)
		}
	}
	d.mu.RUnlock()

	for _, source := range pending {
		allTools, err := source.toolset.Tools(ctx)
		if err != nil {
			slog.DebugContext(ctx, "Deferred source unavailable; skipping", "source", tools.DescribeToolSet(source.toolset), "error", err)
			continue
		}

		var snapshot []tools.Tool
		for _, tool := range allTools {
			if source.defers(tool.Name) {
				snapshot = append(snapshot, tool)
			}
		}

		d.mu.Lock()
		// A concurrent call may have snapshotted the same source; first wins.
		if !source.listed {
			source.snapshot = snapshot
			source.listed = true
		}
		d.mu.Unlock()
	}
}

// deferredTools returns the not-yet-activated deferred tools across all
// listed sources, first source winning on duplicate names.
func (d *ToolSet) deferredTools(ctx context.Context) map[string]tools.Tool {
	d.snapshotPendingSources(ctx)

	d.mu.RLock()
	defer d.mu.RUnlock()

	result := make(map[string]tools.Tool)
	for _, source := range d.sources {
		for _, tool := range source.snapshot {
			if _, active := d.activatedTools[tool.Name]; active {
				continue
			}
			if _, exists := result[tool.Name]; !exists {
				result[tool.Name] = tool
			}
		}
	}
	return result
}

func (d *ToolSet) handleSearchTool(ctx context.Context, args SearchToolArgs) (*tools.ToolCallResult, error) {
	queryRunes := []rune(strings.ToLower(strings.TrimSpace(args.Query)))

	deferredTools := d.deferredTools(ctx)

	type scoredDeferredTool struct {
		result SearchToolResult
		score  int
	}

	var matches []scoredDeferredTool
	for name, tool := range deferredTools {
		if len(queryRunes) == 0 {
			matches = append(matches, scoredDeferredTool{
				result: SearchToolResult{
					Name:        name,
					Description: tool.Description,
				},
				score: 0,
			})
			continue
		}

		chars := util.ToChars([]byte(name + " " + tool.Description))
		res, _ := algo.FuzzyMatchV2(
			false, // caseSensitive
			true,  // normalize
			true,  // forward
			&chars,
			queryRunes,
			true, // withPos
			nil,  // slab
		)

		if res.Start >= 0 {
			matches = append(matches, scoredDeferredTool{
				result: SearchToolResult{
					Name:        name,
					Description: tool.Description,
				},
				score: res.Score,
			})
		}
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].score > matches[j].score
	})

	var results []SearchToolResult
	for _, match := range matches {
		results = append(results, match.result)
	}

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(
			attribute.String("cagent.tool.deferred.op", "search_tool"),
			attribute.String("cagent.tool.deferred.query", args.Query),
			attribute.Int("cagent.tool.deferred.match_count", len(results)),
			attribute.Int("cagent.tool.deferred.pool_size", len(deferredTools)),
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

type addOutcome string

const (
	outcomeActivated     addOutcome = "activated"
	outcomeAlreadyActive addOutcome = "already_active"
	outcomeNotFound      addOutcome = "not_found"
)

func (d *ToolSet) handleAddTool(ctx context.Context, args AddToolArgs) (*tools.ToolCallResult, error) {
	d.snapshotPendingSources(ctx)

	// Decide and apply under one lock so concurrent add_tool calls for the
	// same name report exactly one activation.
	d.mu.Lock()
	outcome, tool := d.activateLocked(args.Name)
	activatedCount := len(d.activatedTools)
	d.mu.Unlock()

	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(
			attribute.String("cagent.tool.deferred.op", "add_tool"),
			attribute.String("cagent.tool.deferred.tool_name", args.Name),
			attribute.String("cagent.tool.deferred.outcome", string(outcome)),
			attribute.Int("cagent.tool.deferred.activated_count", activatedCount),
		)
	}

	switch outcome {
	case outcomeAlreadyActive:
		return tools.ResultSuccess(fmt.Sprintf("Tool '%s' is already active", args.Name)), nil
	case outcomeActivated:
		return tools.ResultSuccess(fmt.Sprintf("Tool '%s' has been activated and is now available for use.\n\nDescription: %s", args.Name, tool.Description)), nil
	default:
		return tools.ResultError(fmt.Sprintf("Tool '%s' not found.", args.Name)), nil
	}
}

// activateLocked activates name from the first listed source that defers it.
// d.mu must be held for writing.
func (d *ToolSet) activateLocked(name string) (addOutcome, tools.Tool) {
	if tool, active := d.activatedTools[name]; active {
		return outcomeAlreadyActive, tool
	}
	for _, source := range d.sources {
		for _, tool := range source.snapshot {
			if tool.Name == name {
				d.activatedTools[name] = tool
				return outcomeActivated, tool
			}
		}
	}
	return outcomeNotFound, tools.Tool{}
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
