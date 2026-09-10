// Package defaults wires docker-agent's full set of toolsets, model providers,
// agent sources and optional loader features into the team loader. Embedders
// that want a smaller binary pass their own registries and options to
// teamloader.Load instead of importing this package.
package defaults

import (
	"github.com/docker/docker-agent/pkg/codingharness"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/sources"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/jscommands"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/teamloader/toolsets"
	"github.com/docker/docker-agent/pkg/tools/builtin/deferred"
	"github.com/docker/docker-agent/pkg/tools/codemode"
	"github.com/docker/docker-agent/pkg/tools/toon"
)

func Opts() []teamloader.Opt {
	// ${...} JavaScript expressions in slash-command instructions and
	// `harness:` agents are runtime features; enable them alongside the
	// loader's own.
	jscommands.Register()
	runtime.RegisterHarness(codingharness.Factory)
	return []teamloader.Opt{
		teamloader.WithToolsetRegistry(toolsets.NewDefaultToolsetRegistry()),
		teamloader.WithProviderRegistry(providers.NewDefaultRegistry()),
		teamloader.WithSourceResolver(resolveExternalAgent),
		teamloader.WithExpander(js.NewJsExpander),
		teamloader.WithCodeMode(codemode.Wrap),
		teamloader.WithToon(toon.Wrap),
		teamloader.WithDeferredTools(deferred.New),
	}
}

// resolveExternalAgent resolves sub-agent references with every source type;
// external agents are never signature-verified, so no OCI options apply.
func resolveExternalAgent(ref string, env environment.Provider) (config.Source, error) {
	return sources.Resolve(ref, env)
}
