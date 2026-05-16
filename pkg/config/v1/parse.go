package v1

import (
	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config/types"
	previous "github.com/docker/docker-agent/pkg/config/v0"
)

func Register(parsers map[string]func([]byte) (any, error), upgraders *[]func(any, []byte) (any, error)) {
	parsers[Version] = func(d []byte) (any, error) { return parse(d) }
	*upgraders = append(*upgraders, upgradeIfNeeded)
}

func parse(data []byte) (Config, error) {
	var cfg Config
	err := yaml.UnmarshalWithOptions(data, &cfg, yaml.Strict())
	return cfg, err
}

func upgradeIfNeeded(c any, _ []byte) (any, error) {
	old, ok := c.(previous.Config)
	if !ok {
		return c, nil
	}

	var config Config
	types.CloneThroughJSON(old, &config)

	// model.Type --> model.Provider
	for name := range old.Models {
		oldModel := old.Models[name]
		newModel := config.Models[name]

		newModel.Provider = oldModel.Type
		config.Models[name] = newModel
	}

	// todo:true --> toolsets: [{type: todo}]
	// think:true --> toolsets: [{type: think}]
	// memory:{path: PATH} --> toolsets: [{type: memory, path: PATH}]
	for name := range old.Agents {
		oldAgent := old.Agents[name]
		newAgent := config.Agents[name]

		var toolsets []Toolset

		if oldAgent.Todo.Enabled {
			toolsets = append(toolsets, Toolset{
				Type:   "todo",
				Shared: oldAgent.Todo.Shared,
			})
		}
		if oldAgent.Think {
			toolsets = append(toolsets, Toolset{
				Type: "think",
			})
		}
		if oldAgent.MemoryConfig.Path != "" {
			toolsets = append(toolsets, Toolset{
				Type: "memory",
				Path: oldAgent.MemoryConfig.Path,
			})
		}

		toolsets = append(toolsets, newAgent.Toolsets...)
		newAgent.Toolsets = toolsets
		config.Agents[name] = newAgent
	}

	return config, nil
}
