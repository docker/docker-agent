package client

import (
	"context"
	"errors"
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/tools"
)

// Creator returns a teamloader.ToolsetCreator using the supplied expander.
// Pass teamloader.NewEnvExpander to support placeholders without JavaScript.
func Creator[E Expander](newExpander func(environment.Provider) E, options ...Option) func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
	return func(_ context.Context, toolset latest.Toolset, _ string, runConfig *config.RuntimeConfig, _ string) (tools.ToolSet, error) {
		return CreateToolSet(toolset, runConfig, newExpander, options...)
	}
}

// CreateToolSet builds a configured HTTP tool with the supplied expander.
func CreateToolSet[E Expander](toolset latest.Toolset, runConfig *config.RuntimeConfig, newExpander func(environment.Provider) E, options ...Option) (tools.ToolSet, error) {
	if toolset.APIConfig.Endpoint == "" {
		return nil, errors.New("api tool requires an endpoint in api_config")
	}
	expander := newExpander(runConfig.EnvProvider())
	var opts []Option
	if toolset.Timeout > 0 {
		opts = append(opts, WithTimeout(time.Duration(toolset.Timeout)*time.Second))
	}
	if toolset.AllowPrivateIPsEnabled() {
		opts = append(opts, WithAllowPrivateIPs(true))
	}
	return New(toolset.APIConfig, expander, append(opts, options...)...), nil
}
