package teamloader

import (
	"cmp"
	"context"
	"fmt"
	"maps"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// ToolsetCreator is a function that creates a toolset based on the provided configuration.
// configName identifies the agent config file (e.g. "memory_agent" from "memory_agent.yaml").
type ToolsetCreator func(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, configName string) (tools.ToolSet, error)

// ToolsetRegistry manages the registration of toolset creators by type.
type ToolsetRegistry interface {
	CreateTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, agentName string) (tools.ToolSet, error)
}

func NewToolsetRegistry(creators map[string]ToolsetCreator) ToolsetRegistry {
	copied := make(map[string]ToolsetCreator, len(creators))
	maps.Copy(copied, creators)
	return &toolsetRegistry{creators: copied}
}

var defaultToolsetCreators map[string]ToolsetCreator

// NewDefaultToolsetRegistry returns the package-level default registry. It is
// empty unless an application explicitly wires creators into this package; YAML
// applications should use pkg/teamloader/toolsets.NewDefaultToolsetRegistry.
func NewDefaultToolsetRegistry() ToolsetRegistry {
	return NewToolsetRegistry(defaultToolsetCreators)
}

// toolsetRegistry manages the registration of toolset creators by type.
type toolsetRegistry struct {
	creators map[string]ToolsetCreator
}

// CreateTool creates a toolset using the registered creator for the given type.
func (r *toolsetRegistry) CreateTool(ctx context.Context, toolset latest.Toolset, parentDir string, runConfig *config.RuntimeConfig, agentName string) (tools.ToolSet, error) {
	creator, ok := r.creators[toolset.Type]
	if !ok {
		return nil, fmt.Errorf("unknown toolset type: %s", toolset.Type)
	}
	ts, err := creator(ctx, toolset, parentDir, runConfig, agentName)
	if err != nil {
		return nil, err
	}
	return tools.WithName(ts, cmp.Or(toolset.Name, toolset.Type)), nil
}
