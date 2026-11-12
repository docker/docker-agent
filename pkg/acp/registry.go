package acp

import (
	"context"
	"os"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/teamloader"
	loadertoolsets "github.com/docker/docker-agent/pkg/teamloader/toolsets"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

// createToolsetRegistry creates a custom toolset registry with ACP-specific filesystem toolset
func createToolsetRegistry(agent *Agent) teamloader.ToolsetRegistry {
	return &acpToolsetRegistry{
		agent:    agent,
		registry: loadertoolsets.NewDefaultToolsetRegistry(),
	}
}

type acpToolsetRegistry struct {
	agent    *Agent
	registry teamloader.ToolsetRegistry
}

func (r *acpToolsetRegistry) CreateTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, agentName string) (tools.ToolSet, error) {
	r.agent.mu.Lock()
	clientTerminal := r.agent.clientTerminal
	r.agent.mu.Unlock()
	if clientTerminal && toolset.Type == "shell" {
		return newTerminalToolset(ctx, toolset, runConfig)
	}
	if clientTerminal && toolset.Type == "environment" {
		return &terminalEnvironment{}, nil
	}
	if toolset.Type == "filesystem" {
		wd := runConfig.WorkingDir
		if wd == "" {
			var err error
			wd, err = os.Getwd()
			if err != nil {
				return nil, err
			}
		}

		base, err := filesystem.NewFromConfig(toolset, runConfig)
		if err != nil {
			return nil, err
		}
		return &FilesystemToolset{ToolSet: base, agent: r.agent, workingDir: wd}, nil
	}

	return r.registry.CreateTool(ctx, toolset, parentDir, runConfig, agentName)
}

func (r *acpToolsetRegistry) Has(toolsetType string) bool {
	return toolsetType == "filesystem" || r.registry.Has(toolsetType)
}
