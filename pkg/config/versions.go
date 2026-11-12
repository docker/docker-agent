package config

import "github.com/docker/docker-agent/pkg/config/latest"

// VersionRegistrar adds parsers and ordered migrations to a load-local registry.
// Registrars must not retain the map or slice; they may be called concurrently.
type VersionRegistrar func(map[string]func([]byte) (any, error), *[]func(any, []byte) (any, error))

// WithVersions enables additional config versions, for example legacy.Register
// from pkg/config/legacy. The default is latest-only. The last option wins;
// nil restores latest-only loading. The latest parser is always registered.
func WithVersions(register VersionRegistrar) LoadOption {
	return func(o *loadOptions) {
		o.versions = register
	}
}

func versions(register VersionRegistrar) (map[string]func([]byte) (any, error), []func(any, []byte) (any, error)) {
	parsers := map[string]func([]byte) (any, error){}
	var upgraders []func(any, []byte) (any, error)
	if register != nil {
		register(parsers, &upgraders)
	}
	latest.Register(parsers, &upgraders)
	return parsers, upgraders
}
