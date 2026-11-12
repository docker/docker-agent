package lsp

import (
	"context"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// Creator builds this toolset from its YAML declaration. It matches
// teamloader.ToolsetCreator so it can be registered directly:
//
//	teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{"lsp": lsp.Creator})
func Creator(ctx context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	return CreateToolSet(ctx, toolset, runConfig)
}
