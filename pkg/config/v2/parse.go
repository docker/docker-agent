package v2

import (
	"errors"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config/types"
	previous "github.com/docker/docker-agent/pkg/config/v1"
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

	if len(old.Env) > 0 {
		return Config{}, errors.New("top-level Env is not supported anymore")
	}

	for i := range old.Models {
		model := old.Models[i]

		if len(model.Env) > 0 {
			return Config{}, errors.New("model Env is not supported anymore")
		}
	}

	for _, agent := range old.Agents {
		for i := range agent.Toolsets {
			toolSet := agent.Toolsets[i]

			if len(toolSet.Envfiles) > 0 {
				return Config{}, errors.New("toolset Envfiles is not supported anymore")
			}
		}
	}

	var config Config
	types.CloneThroughJSON(old, &config)
	return config, nil
}
