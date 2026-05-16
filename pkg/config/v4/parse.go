package v4

import (
	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config/types"
	previous "github.com/docker/docker-agent/pkg/config/v3"
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

func upgradeIfNeeded(c any, raw []byte) (any, error) {
	old, ok := c.(previous.Config)
	if !ok {
		return c, nil
	}

	// Put the agents on the side
	previousAgents := old.Agents
	old.Agents = nil

	var config Config
	types.CloneThroughJSON(old, &config)

	// For agents, we have to read in what they order they appear in the raw config
	type Original struct {
		Agents yaml.MapSlice `yaml:"agents"`
	}

	var original Original
	if err := yaml.Unmarshal(raw, &original); err != nil {
		return nil, err
	}

	for _, agent := range original.Agents {
		name := agent.Key.(string)

		var agentConfig AgentConfig
		types.CloneThroughJSON(previousAgents[name], &agentConfig)
		agentConfig.Name = name

		config.Agents = append(config.Agents, agentConfig)
	}

	return config, nil
}
