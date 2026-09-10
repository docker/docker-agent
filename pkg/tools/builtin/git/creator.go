package git

import (
	"context"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// Creator builds this toolset from its YAML declaration. It matches
// teamloader.ToolsetCreator so it can be registered directly:
//
//	teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{"git": git.Creator})
func Creator(_ context.Context, _ latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
	return CreateToolSet(runConfig)
}
