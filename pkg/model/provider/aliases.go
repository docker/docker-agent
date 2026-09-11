package provider

import (
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chatgpt"
)

// Alias defines the configuration for a provider alias.
type Alias struct {
	APIType string // The actual API type to use (openai, anthropic, etc.)
	// BaseURL is the default base URL for the provider. It may contain
	// ${VAR}/${env.VAR} references (e.g. ${CLOUDFLARE_ACCOUNT_ID}) that are
	// resolved from the runtime environment when the provider is built.
	BaseURL     string
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
	"nvidia": {
		APIType:     "openai",
		BaseURL:     "https://integrate.api.nvidia.com/v1",
		TokenEnvVar: "NVIDIA_API_KEY",
	},
	"openrouter": {
		APIType:     "openai",
		BaseURL:     "https://openrouter.ai/api/v1",
		TokenEnvVar: "OPENROUTER_API_KEY",
	},
	"atlascloud": {
		APIType:     "openai",
		BaseURL:     "https://api.atlascloud.ai/v1",
		TokenEnvVar: "ATLASCLOUD_API_KEY",
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
	"baseten": {
		APIType:     "openai",
		BaseURL:     "https://inference.baseten.co/v1",
		TokenEnvVar: "BASETEN_API_KEY",
	},
	"ovhcloud": {
		APIType:     "openai",
		BaseURL:     "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1",
		TokenEnvVar: "OVH_AI_ENDPOINTS_ACCESS_TOKEN",
	},
	"groq": {
		APIType:     "openai",
		BaseURL:     "https://api.groq.com/openai/v1",
		TokenEnvVar: "GROQ_API_KEY",
	},
	"fireworks": {
		APIType:     "openai",
		BaseURL:     "https://api.fireworks.ai/inference/v1",
		TokenEnvVar: "FIREWORKS_API_KEY",
	},
	"deepseek": {
		APIType:     "openai",
		BaseURL:     "https://api.deepseek.com/v1",
		TokenEnvVar: "DEEPSEEK_API_KEY",
	},
	"cerebras": {
		APIType:     "openai",
		BaseURL:     "https://api.cerebras.ai/v1",
		TokenEnvVar: "CEREBRAS_API_KEY",
	},
	"together": {
		APIType:     "openai",
		BaseURL:     "https://api.together.xyz/v1",
		TokenEnvVar: "TOGETHER_API_KEY",
	},
	"huggingface": {
		APIType:     "openai",
		BaseURL:     "https://router.huggingface.co/v1",
		TokenEnvVar: "HF_TOKEN",
	},
	"moonshot": {
		APIType:     "openai",
		BaseURL:     "https://api.moonshot.ai/v1",
		TokenEnvVar: "MOONSHOT_API_KEY",
	},
	"vercel": {
		APIType:     "openai",
		BaseURL:     "https://ai-gateway.vercel.sh/v1",
		TokenEnvVar: "AI_GATEWAY_API_KEY",
	},
	// Cloudflare endpoints are account-scoped, so their base URLs are templated
	// with ${CLOUDFLARE_ACCOUNT_ID} (and ${CLOUDFLARE_GATEWAY_ID} for the
	// gateway), resolved from the environment at provider-build time.
	"cloudflare-workers-ai": {
		APIType:     "openai",
		BaseURL:     "https://api.cloudflare.com/client/v4/accounts/${CLOUDFLARE_ACCOUNT_ID}/ai/v1",
		TokenEnvVar: "CLOUDFLARE_API_TOKEN",
	},
	"cloudflare-ai-gateway": {
		APIType:     "openai",
		BaseURL:     "https://gateway.ai.cloudflare.com/v1/${CLOUDFLARE_ACCOUNT_ID}/${CLOUDFLARE_GATEWAY_ID}/compat",
		TokenEnvVar: "CLOUDFLARE_API_TOKEN",
	},
	"github-copilot": {
		APIType:     "openai",
		BaseURL:     "https://api.githubcopilot.com",
		TokenEnvVar: "GITHUB_TOKEN",
	},
	// The chatgpt provider uses a ChatGPT subscription (Plus/Pro/Business)
	// instead of an API key: the `docker agent setup` sign-in stores an
	// OAuth login that the "chatgpt-login" environment source exposes as the
	// virtual CHATGPT_OAUTH_TOKEN variable. The Codex backend only serves
	// the Responses API; the OpenAI client pins it for this provider.
	"chatgpt": {
		APIType:     "openai",
		BaseURL:     chatgpt.BaseURL,
		TokenEnvVar: chatgpt.TokenEnvVar,
	},
	"opencode-go": {
		APIType:     "openai",
		BaseURL:     "https://opencode.ai/zen/go/v1",
		TokenEnvVar: "OPENCODE_API_KEY",
	},
	"opencode-zen": {
		APIType:     "openai",
		BaseURL:     "https://opencode.ai/zen/v1",
		TokenEnvVar: "OPENCODE_API_KEY",
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
