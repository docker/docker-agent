package config

import (
	"github.com/docker/docker-agent/pkg/config/latest"
	v0 "github.com/docker/docker-agent/pkg/config/v0"
	v1 "github.com/docker/docker-agent/pkg/config/v1"
	v2 "github.com/docker/docker-agent/pkg/config/v2"
	v3 "github.com/docker/docker-agent/pkg/config/v3"
	v4 "github.com/docker/docker-agent/pkg/config/v4"
	v5 "github.com/docker/docker-agent/pkg/config/v5"
	v6 "github.com/docker/docker-agent/pkg/config/v6"
	v7 "github.com/docker/docker-agent/pkg/config/v7"
	v8 "github.com/docker/docker-agent/pkg/config/v8"
	v9 "github.com/docker/docker-agent/pkg/config/v9"
)

// v9 is the snapshot of the config schema before the harness feature (version "9").
// latest is version "10" which adds the harness: key to AgentConfig.
var _ = v9.Version // ensure v9 is used

func versions() (map[string]func([]byte) (any, error), []func(any, []byte) (any, error)) {
	parsers := map[string]func([]byte) (any, error){}
	var upgraders []func(any, []byte) (any, error)

	v0.Register(parsers, &upgraders)
	v1.Register(parsers, &upgraders)
	v2.Register(parsers, &upgraders)
	v3.Register(parsers, &upgraders)
	v4.Register(parsers, &upgraders)
	v5.Register(parsers, &upgraders)
	v6.Register(parsers, &upgraders)
	v7.Register(parsers, &upgraders)
	v8.Register(parsers, &upgraders)
	v9.Register(parsers, &upgraders)
	latest.Register(parsers, &upgraders)

	return parsers, upgraders
}
