package runtime

import (
	"context"
	"log/slog"
	"slices"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/tools"
)

// nativeToolSearchEnabled reports whether p is opted into provider-hosted
// tool search. Read through BaseConfig so instrumentation wrappers stay
// transparent.
func nativeToolSearchEnabled(p provider.Provider) bool {
	if p == nil {
		return false
	}
	cfg := p.BaseConfig()
	return cfg.NativeToolSearchEnabled()
}

// catalogEnabled reports whether any model this agent may call in a turn has
// hosted tool search; only then do catalog tools join the request set. The
// non-native models of the chain drop them again in toolsForProvider.
func catalogEnabled(a *agent.Agent) bool {
	return slices.ContainsFunc(slices.Concat(a.EffectiveModels(), a.FallbackModels()), nativeToolSearchEnabled)
}

// listAgentTools lists the agent's tools, catalog included when a model of the
// chain can search it.
func listAgentTools(ctx context.Context, a *agent.Agent) ([]tools.Tool, error) {
	if catalogEnabled(a) {
		return a.ToolsWithCatalog(ctx)
	}
	return a.Tools(ctx)
}

// toolsForProvider drops search-only catalog tools for a provider without
// hosted tool search, so a fallback or switched model keeps the legacy
// search_tool/add_tool surface instead of receiving the whole catalog.
func toolsForProvider(ctx context.Context, p provider.Provider, agentTools []tools.Tool) []tools.Tool {
	if nativeToolSearchEnabled(p) {
		return agentTools
	}
	filtered := tools.WithoutSearchOnly(agentTools)
	if len(filtered) != len(agentTools) {
		slog.DebugContext(ctx, "Dropping search-only catalog tools for a model without hosted tool search",
			"model", p.ID().String(), "dropped", len(agentTools)-len(filtered))
	}
	return filtered
}
