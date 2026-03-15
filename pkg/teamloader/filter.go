package teamloader

import (
	"context"
	"log/slog"
	"slices"

	"github.com/docker/docker-agent/pkg/tools"
)

// WithToolsFilter creates a toolset that only includes the specified tools.
// If no tool names are provided, all tools are included.
func WithToolsFilter(inner tools.ToolSet, toolNames ...string) tools.ToolSet {
	if len(toolNames) == 0 {
		return inner
	}

	return &filterTools{
		ToolSet:   inner,
		toolNames: toolNames,
		exclude:   false,
	}
}

// WithToolsExcludeFilter creates a toolset that excludes the specified tools.
// If no tool names are provided, all tools are included.
func WithToolsExcludeFilter(inner tools.ToolSet, toolNames ...string) tools.ToolSet {
	if len(toolNames) == 0 {
		return inner
	}

	return &filterTools{
		ToolSet:   inner,
		toolNames: toolNames,
		exclude:   true,
	}
}

type filterTools struct {
	tools.ToolSet
	toolNames []string
	exclude   bool
}

// Verify interface compliance
var (
	_ tools.Instructable = (*filterTools)(nil)
	_ tools.Unwrapper    = (*filterTools)(nil)
)

// Unwrap implements tools.Unwrapper.
func (f *filterTools) Unwrap() tools.ToolSet {
	return f.ToolSet
}

// Start forwards the Start call to the inner toolset if it implements Startable.
// This is necessary because filterTools wraps toolsets (like MCP) that require
// initialization before their Tools() method can be called.
func (f *filterTools) Start(ctx context.Context) error {
	if startable, ok := f.ToolSet.(tools.Startable); ok {
		return startable.Start(ctx)
	}
	return nil
}

// Stop forwards the Stop call to the inner toolset if it implements Startable.
func (f *filterTools) Stop(ctx context.Context) error {
	if startable, ok := f.ToolSet.(tools.Startable); ok {
		return startable.Stop(ctx)
	}
	return nil
}

// Instructions implements tools.Instructable by delegating to the inner toolset.
func (f *filterTools) Instructions() string {
	return tools.GetInstructions(f.ToolSet)
}

func (f *filterTools) Tools(ctx context.Context) ([]tools.Tool, error) {
	allTools, err := f.ToolSet.Tools(ctx)
	if err != nil {
		return nil, err
	}

	var filtered []tools.Tool
	for _, tool := range allTools {
		contains := slices.Contains(f.toolNames, tool.Name)

		// Exclude mode: keep only tools NOT in the list
		// Include mode: keep only tools in the list
		if (f.exclude && contains) || (!f.exclude && !contains) {
			slog.Debug("Filtering out tool", "tool", tool.Name)
			continue
		}

		filtered = append(filtered, tool)
	}

	return filtered, nil
}
