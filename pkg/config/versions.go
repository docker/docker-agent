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
)

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
	latest.Register(parsers, &upgraders)

	return parsers, upgraders
}
