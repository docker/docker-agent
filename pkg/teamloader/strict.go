package teamloader

import (
	"fmt"
	"strings"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider"
)

// checkResolvedModels is the second half of WithStrict: config.Requires
// skips models whose provider is only known once the environment is
// consulted, so after first_available selectors are resolved this checks
// them, and resolves each agent's `auto` model eagerly to check the provider
// it lands on.
func checkResolvedModels(cfg *latest.Config, firstAvailableSelectors map[string]bool, opts *loadOptions, autoModel func() latest.ModelConfig) error {
	reqs := config.Requirements{
		Providers: map[string][]string{},
		Toolsets:  map[string][]string{},
		Features:  map[config.Feature][]string{},
	}

	for name := range firstAvailableSelectors {
		m := cfg.Models[name]
		if m.Provider == "" {
			continue
		}
		resolved := provider.ResolveType(&m, cfg.Providers)
		loc := fmt.Sprintf("models.%s (first_available → %s/%s)", name, m.Provider, m.Model)
		reqs.Providers[resolved] = append(reqs.Providers[resolved], loc)
	}

	for _, a := range cfg.Agents {
		if a.Harness != nil {
			continue
		}
		for name := range strings.SplitSeq(a.Model, ",") {
			if name != "auto" {
				continue
			}
			m := autoModel()
			resolved := provider.ResolveType(&m, cfg.Providers)
			loc := fmt.Sprintf("agents.%s.model (auto → %s/%s)", a.Name, m.Provider, m.Model)
			reqs.Providers[resolved] = append(reqs.Providers[resolved], loc)
		}
	}

	return reqs.Check(opts.providerRegistry.Has, opts.toolsetRegistry.Has, opts.features)
}
