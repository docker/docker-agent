package latest

import (
	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config/types"
	v8 "github.com/docker/docker-agent/pkg/config/v8"
	v9 "github.com/docker/docker-agent/pkg/config/v9"
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
	// Upgrade from v9 (schema version "9", pre-harness) to v10 (current).
	// v9 configs have no harness: key; they upgrade cleanly via JSON clone.
	if old, ok := c.(v9.Config); ok {
		var config Config
		types.CloneThroughJSON(old, &config)
		return config, nil
	}
	// Upgrade from v8 directly (in case v9 upgrader was skipped).
	if old, ok := c.(v8.Config); ok {
		var config Config
		types.CloneThroughJSON(old, &config)
		return config, nil
	}
	return c, nil
}
