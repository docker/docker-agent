package v7

import (
	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config/types"
	previous "github.com/docker/docker-agent/pkg/config/v6"
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

	// Migrate AgentConfig.RAG []string → toolsets with type: rag + ref
	for i, agent := range old.Agents {
		if len(agent.RAG) == 0 {
			continue
		}
		for _, ragName := range agent.RAG {
			config.Agents[i].Toolsets = append(config.Agents[i].Toolsets, Toolset{
				Type: "rag",
				Ref:  ragName,
			})
		}
	}

	// Migrate top-level RAG map from RAGConfig to RAGToolset
	if len(old.RAG) > 0 && config.RAG == nil {
		config.RAG = make(map[string]RAGToolset)
	}
	for name, oldRAG := range old.RAG {
		var ragCfg RAGConfig
		types.CloneThroughJSON(oldRAG, &ragCfg)
		config.RAG[name] = RAGToolset{
			Toolset: Toolset{
				Type:      "rag",
				RAGConfig: &ragCfg,
			},
		}
	}

	return config, nil
}
