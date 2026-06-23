package defaults

import (
	"github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/teamloader/toolsets"
)

func Opts() []teamloader.Opt {
	return []teamloader.Opt{
		teamloader.WithToolsetRegistry(toolsets.NewDefaultToolsetRegistry()),
		teamloader.WithProviderRegistry(providers.NewDefaultRegistry()),
	}
}
