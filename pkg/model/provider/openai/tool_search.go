package openai

import (
	"cmp"
	"context"
	"log/slog"
	"slices"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/docker/docker-agent/pkg/tools"
)

// hostedToolSearchTools prepares requestTools for a Responses request.
//
// With hosted tool search on (provider_opts.native_tool_search on a verified
// model), catalog tools become defer_loading function declarations returned
// in hosted, next to a server-executed tool_search tool; the server owns
// discovery, so every client-side load mark is cleared from regular and
// injectDeferredToolLoads has nothing to inject. hosted is sorted by name:
// activation moves a catalog tool within requestTools, and the declaration
// must stay byte-identical regardless. Otherwise search-only tools are
// dropped and the model keeps the legacy search_tool/add_tool surface, with
// activated catalog tools as regular tools. requestTools is never mutated:
// the runtime shares it across the fallback chain.
func (c *Client) hostedToolSearchTools(ctx context.Context, requestTools []tools.Tool) (regular []tools.Tool, hosted []responses.ToolUnionParam, err error) {
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
		hosted = append(hosted, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name:         tool.Name,
			Description:  param.NewOpt(tool.Description),
			Parameters:   parameters,
			Strict:       param.NewOpt(strict),
			DeferLoading: param.NewOpt(true),
		}})
	}
	slices.SortFunc(hosted, func(a, b responses.ToolUnionParam) int { return cmp.Compare(a.OfFunction.Name, b.OfFunction.Name) })
	hosted = append(hosted, responses.ToolUnionParam{OfToolSearch: &responses.ToolSearchToolParam{
		Execution: responses.ToolSearchToolExecutionServer,
	}})
	slog.DebugContext(ctx, "Using hosted tool search", "model", c.ModelConfig.Model, "deferred_tool_count", len(hosted)-1)
	return regular, hosted, nil
}
