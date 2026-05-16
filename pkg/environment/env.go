package environment

import (
	"context"
	"os"
	"strings"
)

// OsEnvProvider provides access to the operating system's environment variables.
type OsEnvProvider struct{}

func NewOsEnvProvider() *OsEnvProvider {
	return &OsEnvProvider{}
}

func (p *OsEnvProvider) Get(_ context.Context, name string) (string, bool) {
	return os.LookupEnv(name)
}

// EnvListProvider provides access a list of environment variables.
type EnvListProvider struct {
	env []string
}

func NewEnvListProvider(env []string) *EnvListProvider {
	return &EnvListProvider{
		env: env,
	}
}

func (p *EnvListProvider) Get(_ context.Context, name string) (string, bool) {
	for _, e := range p.env {
		n, v, ok := strings.Cut(e, "=")
		if ok && n == name {
			return v, true
		}
	}
	return "", false
}

// MapEnvProvider provides access to a static map of environment variables.
type MapEnvProvider struct {
	vars map[string]string
}

func NewMapEnvProvider(vars map[string]string) *MapEnvProvider {
	return &MapEnvProvider{vars: vars}
}

func (p *MapEnvProvider) Get(_ context.Context, name string) (string, bool) {
	v, ok := p.vars[name]
	return v, ok
}

// NoEnvProvider is a provider that never finds any variable.
type NoEnvProvider struct{}

func NewNoEnvProvider() *NoEnvProvider {
	return &NoEnvProvider{}
}

func (p *NoEnvProvider) Get(context.Context, string) (string, bool) {
	return "", false
}

// EnvFilesProvider provides access env files.
type EnvFilesProvider struct {
	values []KeyValuePair
}

func NewEnvFilesProvider(absEnvFiles []string) (*EnvFilesProvider, error) {
	values, err := ReadEnvFiles(absEnvFiles)
	if err != nil {
		return nil, err
	}

	return &EnvFilesProvider{
		values: values,
	}, nil
}

func (p *EnvFilesProvider) Get(_ context.Context, name string) (string, bool) {
	for _, kv := range p.values {
		if kv.Key == name {
			return kv.Value, true
		}
	}

	return "", false
}
