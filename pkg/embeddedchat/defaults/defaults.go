// Package defaults provides docker-agent's full toolset and provider
// registries for embeddedchat's AgentSource path. It is a separate package so
// embedders that supply a pre-built team (Config.Team) never link the full
// registries into their binary.
package defaults

import (
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
)

// Opts returns team-loader options with docker-agent's full toolset and
// provider registries, for embeddedchat.Config.LoadOpts.
func Opts() []teamloader.Opt {
	return loaderdefaults.Opts()
}
