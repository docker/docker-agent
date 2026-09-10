// Package api provides HTTP API tools with JavaScript template expansion.
// Import api/client instead to supply an expander without linking JavaScript.
package api

import (
	"time"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/api/client"
	"github.com/docker/docker-agent/pkg/upstream"
)

type (
	ToolSet = client.ToolSet
	Option  = client.Option
)

// CreateToolSet is used by the tools registry.
func CreateToolSet(toolset latest.Toolset, runConfig *config.RuntimeConfig) (tools.ToolSet, error) {
	return client.CreateToolSet(toolset, runConfig, js.NewJsExpander, client.WithHeaderResolver(upstream.ResolveHeaders))
}

func WithTimeout(d time.Duration) Option    { return client.WithTimeout(d) }
func WithAllowPrivateIPs(allow bool) Option { return client.WithAllowPrivateIPs(allow) }

func New(apiConfig latest.APIToolConfig, expander *js.Expander, opts ...Option) *ToolSet {
	return client.New(apiConfig, expander, append(opts, client.WithHeaderResolver(upstream.ResolveHeaders))...)
}
