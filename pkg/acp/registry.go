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
	if toolset.Type == "filesystem" {
		wd := runConfig.WorkingDir
		if wd == "" {
			var err error
			wd, err = os.Getwd()
			if err != nil {
				return nil, err
			}
		}

		return NewFilesystemToolset(r.agent, wd, filesystemOptions(toolset)...), nil
	}

	return r.registry.CreateTool(ctx, toolset, parentDir, runConfig, agentName)
}

func filesystemOptions(toolset latest.Toolset) []filesystem.Opt {
	opts := []filesystem.Opt{}

	ignoreVCS := true
	if toolset.IgnoreVCS != nil {
		ignoreVCS = *toolset.IgnoreVCS
	}
	opts = append(opts, filesystem.WithIgnoreVCS(ignoreVCS))

	if len(toolset.AllowList) > 0 {
		opts = append(opts, filesystem.WithAllowList(toolset.AllowList))
	}
	if len(toolset.DenyList) > 0 {
		opts = append(opts, filesystem.WithDenyList(toolset.DenyList))
	}

	if len(toolset.PostEdit) > 0 {
		postEditConfigs := make([]filesystem.PostEditConfig, len(toolset.PostEdit))
		for i, pe := range toolset.PostEdit {
			postEditConfigs[i] = filesystem.PostEditConfig{
				Path: pe.Path,
				Cmd:  pe.Cmd,
			}
		}
		opts = append(opts, filesystem.WithPostEditCommands(postEditConfigs))
	}

	return opts
}
