package acp

import (
	"context"
	"os"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

// createToolsetRegistry creates a custom toolset registry with ACP-specific filesystem toolset
func createToolsetRegistry(agent *Agent) *teamloader.ToolsetRegistry {
	registry := teamloader.NewDefaultToolsetRegistry()

	registry.Register("filesystem", func(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
		wd := runConfig.WorkingDir
		if wd == "" {
			var err error
			wd, err = os.Getwd()
			if err != nil {
				return nil, err
			}
		}

		return NewFilesystemToolset(agent, wd), nil
	})

	return registry
}
