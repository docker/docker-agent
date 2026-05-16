package v8

import (
	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config/types"
	previous "github.com/docker/docker-agent/pkg/config/v7"
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
	return config, nil
}
