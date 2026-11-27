package openai

import (
	"cmp"
	"context"
	"log/slog"
	"slices"

	"github.com/docker/docker-agent/pkg/tools"
)

func (c *Client) hostedToolSearchTools(ctx context.Context, requestTools []tools.Tool) (regular []tools.Tool, hosted []ResponseTool, err error) {
	if !slices.ContainsFunc(requestTools, func(t tools.Tool) bool { return t.InCatalog }) {
		return requestTools, nil, nil
	}
	if !c.NativeToolSearchEnabled() {
		slog.DebugContext(ctx, "Dropping search-only catalog tools: hosted tool search is off for this model", "model", c.ModelConfig.Model)
		return tools.WithoutSearchOnly(requestTools), nil, nil
	}

	regular = make([]tools.Tool, 0, len(requestTools))
	for _, tool := range requestTools {
		if !tool.InCatalog {
			tool.Deferred = false
			tool.DeferredAtToolCallID = ""
			regular = append(regular, tool)
			continue
		}
		parameters, strict, err := ConvertParametersToSchema(tool.Parameters)
		if err != nil {
			return nil, nil, err
		}
		tools.OrderedSchemaProperties(parameters)
		hosted = append(hosted, ResponseTool{
			Type:         "function",
			Name:         tool.Name,
			Description:  tool.Description,
			Parameters:   parameters,
			Strict:       new(strict),
			DeferLoading: new(true),
		})
	}
	slices.SortFunc(hosted, func(a, b ResponseTool) int { return cmp.Compare(a.Name, b.Name) })
	hosted = append(hosted, ResponseTool{
		Type:      "tool_search",
		Execution: "server",
	})
	slog.DebugContext(ctx, "Using hosted tool search", "model", c.ModelConfig.Model, "deferred_tool_count", len(hosted)-1)
	return regular, hosted, nil
}
