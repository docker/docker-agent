package environment

import (
	"context"
	"strings"
)

// EnvOverridePrefix marks a variable that overrides its unprefixed name for
// docker-agent only, e.g. DOCKER_AGENT_ENV_ANTHROPIC_API_KEY over
// ANTHROPIC_API_KEY. It is a sub-namespace of settingsPrefix so overrides never
// collide with docker-agent's own settings.
const EnvOverridePrefix = "DOCKER_AGENT_ENV_"

const settingsPrefix = "DOCKER_AGENT_"

// PrefixedProvider decorates another Provider so that DOCKER_AGENT_ENV_<NAME>
// shadows <NAME>. This keeps values other tools cannot read (such as op://
// references) out of variables those tools share with docker-agent.
type PrefixedProvider struct {
	providers []Provider
	chain     Provider
}

// NewPrefixedProvider looks providers up in order, overrides first.
func NewPrefixedProvider(providers ...Provider) Provider {
	return &PrefixedProvider{providers: providers, chain: NewMultiProvider(providers...)}
}

// Get skips empty overrides source by source, so a blank line or `export X=`
// hides neither an override in a later source nor a valid <NAME>.
func (p *PrefixedProvider) Get(ctx context.Context, name string) (string, bool) {
	names := LookupNames(name)
	for _, candidate := range names[:len(names)-1] {
		for _, provider := range p.providers {
			if value, found := provider.Get(ctx, candidate); found && value != "" {
				return value, true
			}
		}
	}
	return p.chain.Get(ctx, name)
}

// LookupNames returns the names checked for name, highest precedence first;
// the last one is always name itself.
func LookupNames(name string) []string {
	if strings.HasPrefix(name, settingsPrefix) {
		return []string{name}
	}
	return []string{EnvOverridePrefix + name, name}
}
