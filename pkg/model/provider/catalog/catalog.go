// Package catalog provides embedded metadata for built-in model providers,
// without linking their SDK implementations.
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/modelsdev"
)

// Provider describes a provider's API and transport defaults.
// Callers must treat maps and slices returned by this package as read-only.
type Provider struct {
	// Name is the canonical provider ID used in model references and catalog lookups.
	Name string `json:"name"`
	// APIType selects the registered API implementation that serves this provider.
	APIType string `json:"api_type"`
	// BaseURL is the default endpoint; API implementations resolve dynamic endpoints themselves.
	// Environment references such as ${CLOUDFLARE_ACCOUNT_ID} expand at construction time.
	BaseURL string `json:"base_url,omitempty"`
	// TokenEnvVars lists token sources in priority order; explicit token_key overrides the list.
	TokenEnvVars []string `json:"token_env_vars,omitempty"`
	// DefaultModel is used when selecting this provider without an explicit model.
	// Providers without one are excluded from automatic selection.
	DefaultModel string `json:"default_model,omitempty"`
	// RequiresConfiguration excludes providers needing a user-supplied endpoint from catalog listings and automatic selection.
	RequiresConfiguration bool `json:"requires_configuration,omitempty"`
	// MergeConsecutiveMessages coalesces adjacent same-role Chat Completions messages
	// for servers whose chat templates reject multiple system messages.
	MergeConsecutiveMessages bool `json:"merge_consecutive_messages,omitempty"`
	// AutoResponses selects the Responses API for supported models unless api_type is explicit.
	AutoResponses bool `json:"auto_responses,omitempty"`
	// OpenAIVendor identifies genuine OpenAI model hosts, gating OpenAI-only wire fields.
	// Speaking the OpenAI-compatible protocol alone does not qualify.
	OpenAIVendor bool `json:"openai_vendor,omitempty"`
	// Headers supplies default HTTP headers; user http_headers override them case-insensitively.
	Headers map[string]string `json:"headers,omitempty"`
	// QueryParams maps string-valued provider_opts keys to URL query parameter names.
	QueryParams map[string]string `json:"query_params,omitempty"`
	// Hint describes credential setup in automatic-selection errors; defaults to the token variable names.
	Hint string `json:"hint,omitempty"`
}

//go:embed providers.json
var providersJSON []byte

var orderedProviders = loadProviders()

func loadProviders() []Provider {
	var providers []Provider
	if err := json.Unmarshal(providersJSON, &providers); err != nil {
		panic(fmt.Errorf("decode embedded providers: %w", err))
	}
	return providers
}

// Providers indexes built-in providers by name. Callers must not modify it.
var Providers = func() map[string]Provider {
	providers := make(map[string]Provider, len(orderedProviders))
	for _, p := range orderedProviders {
		providers[p.Name] = p
	}
	return providers
}()

// Ordered returns providers in JSON order, which determines selection priority.
func Ordered() []Provider {
	return orderedProviders
}

// DefaultModels returns the configured default models, keyed by provider name.
func DefaultModels() map[string]string {
	models := make(map[string]string)
	for name, p := range Providers {
		if p.DefaultModel != "" {
			models[name] = p.DefaultModel
		}
	}
	return models
}

// CredentialProviders returns providers with a default model and credential
// detection, in catalog order. Local providers are handled by discovery.
func CredentialProviders() []Provider {
	var providers []Provider
	for _, p := range Ordered() {
		if p.DefaultModel == "" || p.RequiresConfiguration {
			continue
		}
		if len(p.TokenEnvVars) == 0 {
			continue
		}
		if p.Hint == "" {
			p.Hint = p.TokenEnvVars[0]
			if len(p.TokenEnvVars) > 1 {
				p.Hint += " (or " + strings.Join(p.TokenEnvVars[1:], ", ") + ")"
			}
		}
		providers = append(providers, p)
	}
	return providers
}

// Lookup resolves canonical and legacy provider names. Unknown names return zero metadata.
func Lookup(name string) Provider {
	p, _ := Find(name)
	return p
}

// Find returns metadata and whether the provider is registered.
func Find(name string) (Provider, bool) {
	p, ok := Providers[modelsdev.CanonicalProviderID(name)]
	return p, ok
}

// IsDefaultEndpoint treats an omitted endpoint and the catalog endpoint equally.
func IsDefaultEndpoint(name, endpoint string) bool {
	return endpoint == "" || strings.TrimRight(endpoint, "/") == strings.TrimRight(Lookup(name).BaseURL, "/")
}

// TokenKeys returns credential sources in priority order. Alternatives only apply
// to the built-in token key, never to an explicitly overridden credential.
func TokenKeys(name, tokenKey string) []string {
	keys := []string{tokenKey}
	p := Lookup(name)
	if tokenKey != "" && len(p.TokenEnvVars) > 0 && tokenKey == p.TokenEnvVars[0] {
		keys = p.TokenEnvVars
	}
	return keys
}

// AllProviders returns all built-in provider names, sorted.
func AllProviders() []string {
	return slices.Sorted(maps.Keys(Providers))
}

// IsKnownProvider reports whether the name identifies a built-in provider.
func IsKnownProvider(name string) bool {
	_, exists := Find(strings.ToLower(name))
	return exists
}

// IsCatalogProvider reports whether a provider is usable without explicit endpoint configuration.
func IsCatalogProvider(name string) bool {
	p, exists := Find(name)
	return exists && !p.RequiresConfiguration
}
