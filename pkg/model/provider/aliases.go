package provider

import (
	"iter"
	"maps"
	"slices"
	"strings"
)

// Alias defines the configuration for a provider alias.
type Alias struct {
	APIType     string // The actual API type to use (openai, anthropic, etc.)
	BaseURL     string // Default base URL for the provider
	TokenEnvVar string // Environment variable name for the API token
}

// CoreProviders lists all natively implemented provider types.
// These are the provider types that have direct implementations (not aliases).
var CoreProviders = []string{
	"openai",
	"anthropic",
	"google",
	"dmr",
	"amazon-bedrock",
}

// Aliases maps provider names to their corresponding configurations.
//
// Most consumers should call [LookupAlias] for a single lookup or [EachAlias]
// to iterate, both of which keep the rest of the codebase decoupled from this
// concrete map. Direct mutation of Aliases is not supported.
var Aliases = map[string]Alias{
	"requesty": {
		APIType:     "openai",
		BaseURL:     "https://router.requesty.ai/v1",
		TokenEnvVar: "REQUESTY_API_KEY",
	},
	"azure": {
		APIType:     "openai",
		TokenEnvVar: "AZURE_API_KEY",
	},
	"xai": {
		APIType:     "openai",
		BaseURL:     "https://api.x.ai/v1",
		TokenEnvVar: "XAI_API_KEY",
	},
	"nebius": {
		APIType:     "openai",
		BaseURL:     "https://api.studio.nebius.com/v1",
		TokenEnvVar: "NEBIUS_API_KEY",
	},
	"mistral": {
		APIType:     "openai",
		BaseURL:     "https://api.mistral.ai/v1",
		TokenEnvVar: "MISTRAL_API_KEY",
	},
	"ollama": {
		APIType: "openai",
		BaseURL: "http://localhost:11434/v1",
	},
	"minimax": {
		APIType:     "openai",
		BaseURL:     "https://api.minimax.io/v1",
		TokenEnvVar: "MINIMAX_API_KEY",
	},
	"github-copilot": {
		APIType:     "openai",
		BaseURL:     "https://api.githubcopilot.com",
		TokenEnvVar: "GITHUB_TOKEN",
	},
}

// LookupAlias returns the Alias registered for the given name (if any).
// Lookup is case-sensitive; callers that need case-insensitive matching
// should normalise the name first (e.g. [strings.ToLower]).
func LookupAlias(name string) (Alias, bool) {
	alias, ok := Aliases[name]
	return alias, ok
}

// EachAlias returns an iterator over every registered (name, Alias) pair.
// Iteration order is not guaranteed; callers that need a deterministic order
// should sort by name.
func EachAlias() iter.Seq2[string, Alias] {
	return func(yield func(string, Alias) bool) {
		for name, alias := range Aliases {
			if !yield(name, alias) {
				return
			}
		}
	}
}

// AllProviders returns all known provider names (core providers + aliases),
// sorted for deterministic output.
func AllProviders() []string {
	providers := slices.Concat(CoreProviders, slices.Collect(maps.Keys(Aliases)))
	slices.Sort(providers)
	return providers
}

// IsKnownProvider returns true if the provider name is a core provider or an alias.
func IsKnownProvider(name string) bool {
	if slices.Contains(CoreProviders, strings.ToLower(name)) {
		return true
	}
	_, exists := LookupAlias(strings.ToLower(name))
	return exists
}

// CatalogProviders returns the list of provider names that should be shown in the model catalog.
// This includes core providers and aliases that have a defined BaseURL (self-contained endpoints).
// Aliases without a BaseURL (like azure) require user configuration and are excluded.
func CatalogProviders() []string {
	providers := make([]string, 0, len(CoreProviders)+len(Aliases))

	// Add all core providers
	providers = append(providers, CoreProviders...)

	// Add aliases that have a defined BaseURL (they work out of the box)
	for name, alias := range EachAlias() {
		if alias.BaseURL != "" {
			providers = append(providers, name)
		}
	}

	return providers
}

// IsCatalogProvider returns true if the provider name is valid for the model catalog.
func IsCatalogProvider(name string) bool {
	// Check core providers
	if slices.Contains(CoreProviders, name) {
		return true
	}
	// Check aliases with BaseURL
	if alias, exists := LookupAlias(name); exists && alias.BaseURL != "" {
		return true
	}
	return false
}
