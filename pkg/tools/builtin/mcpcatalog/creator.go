package mcpcatalog

import (
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

// CreateToolSet builds the toolset from its YAML declaration; register it
// with teamloader.CreatorFromToolset.
func CreateToolSet(toolset latest.Toolset) (tools.ToolSet, error) {
	var opts []Option
	if len(toolset.AllowedServers) > 0 {
		opts = append(opts, WithAllowedServers(toolset.AllowedServers))
	}
	if len(toolset.BlockedServers) > 0 {
		opts = append(opts, WithBlockedServers(toolset.BlockedServers))
	}
	return New(opts...), nil
}
