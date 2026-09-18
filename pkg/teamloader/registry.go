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
	// Has reports whether a creator is registered for toolsetType.
	Has(toolsetType string) bool
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

func (r *toolsetRegistry) Has(toolsetType string) bool {
	_, ok := r.creators[toolsetType]
	return ok
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

// Creator adapts a toolset constructor that needs neither its YAML
// declaration nor the runtime config into a ToolsetCreator:
//
//	teamloader.NewToolsetRegistry(map[string]teamloader.ToolsetCreator{"think": teamloader.Creator(think.CreateToolSet)})
//
// Packages whose constructors take more (shell, mcp, filesystem, ...) export
// a ready-made Creator instead.
func Creator(create func() (tools.ToolSet, error)) ToolsetCreator {
	return func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
		return create()
	}
}

// CreatorFromToolset adapts a constructor that only needs the YAML
// declaration (todo, model_picker, mcp_catalog) into a ToolsetCreator.
func CreatorFromToolset(create func(latest.Toolset) (tools.ToolSet, error)) ToolsetCreator {
	return func(_ context.Context, toolset latest.Toolset, _ string, _ *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
		return create(toolset)
	}
}
