package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/chatgpt"
	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
)

// DMRModelLister returns the IDs of the models currently available to Docker
// Model Runner (i.e. pulled locally). It is injected so DMR discovery can be
// stubbed in tests and disabled by callers that must stay side-effect-free:
// `docker agent models` passes nil to avoid shelling out to `docker model`,
// while the agent run path passes dmr.ListModels.
type DMRModelLister func(ctx context.Context) ([]string, error)

// providerConfig defines a cloud provider and how to detect/describe its API keys.
type providerConfig struct {
	name    string   // provider name (e.g., "anthropic")
	envVars []string // env vars to check - provider is available if ANY is set
	hint    string   // description for error messages
	// apiKeyEnvVar is the single secret API-key env var that authenticates this
	// provider and is safe to forward into an isolated environment (e.g. an eval
	// container). It is empty when the provider has no forwardable single secret
	// (e.g. amazon-bedrock's multi-variable AWS credentials, or DMR which needs
	// none). It intentionally differs from envVars, which also contains non-secret
	// detection/mode flags (e.g. GOOGLE_GENAI_USE_VERTEXAI) that must never be
	// forwarded as credentials.
	apiKeyEnvVar string
}

// cloudProviders defines the available cloud providers in priority order.
// The first provider with a configured API key will be selected by AutoModelConfig.
// DMR is always appended as the final fallback (not listed here).
//
// opencode-zen is ordered before opencode-go because both share OPENCODE_API_KEY:
// when the key is set, Zen wins auto-selection. A subscriber who only uses Go
// should set the provider explicitly (e.g. `--model opencode-go/...`) rather than
// relying on auto; see docs/providers/opencode-go for details.
var cloudProviders = []providerConfig{
	{"anthropic", []string{"ANTHROPIC_API_KEY"}, "ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY"},
	{"openai", []string{"OPENAI_API_KEY"}, "OPENAI_API_KEY", "OPENAI_API_KEY"},
	// The chatgpt credential is the OAuth login stored by the `docker agent
	// setup` sign-in, surfaced as a virtual env var by the "chatgpt-login"
	// source. It is ordered after openai so adding a ChatGPT sign-in never
	// changes auto-selection for users that already export OPENAI_API_KEY.
	{"chatgpt", []string{chatgpt.TokenEnvVar}, "ChatGPT account sign-in", ""},
	{"github-copilot", []string{"GITHUB_TOKEN", "GH_TOKEN"}, "GITHUB_TOKEN (or GH_TOKEN)", ""},
	{"google", []string{
		"GOOGLE_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_GENAI_USE_VERTEXAI",
	}, "GOOGLE_API_KEY (or GEMINI_API_KEY, GOOGLE_GENAI_USE_VERTEXAI)", "GOOGLE_API_KEY"},
	{"mistral", []string{"MISTRAL_API_KEY"}, "MISTRAL_API_KEY", "MISTRAL_API_KEY"},
	{"openrouter", []string{"OPENROUTER_API_KEY"}, "OPENROUTER_API_KEY", "OPENROUTER_API_KEY"},
	{"baseten", []string{"BASETEN_API_KEY"}, "BASETEN_API_KEY", "BASETEN_API_KEY"},
	{"ovhcloud", []string{"OVH_AI_ENDPOINTS_ACCESS_TOKEN"}, "OVH_AI_ENDPOINTS_ACCESS_TOKEN", "OVH_AI_ENDPOINTS_ACCESS_TOKEN"},
	{"groq", []string{"GROQ_API_KEY"}, "GROQ_API_KEY", "GROQ_API_KEY"},
	{"fireworks", []string{"FIREWORKS_API_KEY"}, "FIREWORKS_API_KEY", "FIREWORKS_API_KEY"},
	{"deepseek", []string{"DEEPSEEK_API_KEY"}, "DEEPSEEK_API_KEY", "DEEPSEEK_API_KEY"},
	{"cerebras", []string{"CEREBRAS_API_KEY"}, "CEREBRAS_API_KEY", "CEREBRAS_API_KEY"},
	{"together", []string{"TOGETHER_API_KEY"}, "TOGETHER_API_KEY", "TOGETHER_API_KEY"},
	{"huggingface", []string{"HF_TOKEN"}, "HF_TOKEN", "HF_TOKEN"},
	{"moonshot", []string{"MOONSHOT_API_KEY"}, "MOONSHOT_API_KEY", "MOONSHOT_API_KEY"},
	{"vercel", []string{"AI_GATEWAY_API_KEY"}, "AI_GATEWAY_API_KEY", "AI_GATEWAY_API_KEY"},
	{"amazon-bedrock", []string{
		"AWS_BEARER_TOKEN_BEDROCK",
		"AWS_ACCESS_KEY_ID",
		"AWS_PROFILE",
		"AWS_ROLE_ARN",
	}, "AWS_ACCESS_KEY_ID (or AWS_PROFILE, AWS_ROLE_ARN, AWS_BEARER_TOKEN_BEDROCK)", ""},
	{"opencode-zen", []string{"OPENCODE_API_KEY"}, "OPENCODE_API_KEY", "OPENCODE_API_KEY"},
	{"opencode-go", []string{"OPENCODE_API_KEY"}, "OPENCODE_API_KEY", "OPENCODE_API_KEY"},
}

// ProviderEnvVars associates a cloud provider known to auto-selection with
// the environment variables that can supply its credentials.
type ProviderEnvVars struct {
	Provider string
	EnvVars  []string
}

// CloudProviderEnvVars returns, in auto-selection priority order, every cloud
// provider with the environment variables that can hold its credentials.
// Diagnostic commands (e.g. `docker agent doctor`) use it to report credential
// state without duplicating the provider table.
func CloudProviderEnvVars() []ProviderEnvVars {
	out := make([]ProviderEnvVars, 0, len(cloudProviders))
	for _, p := range cloudProviders {
		out = append(out, ProviderEnvVars{Provider: p.name, EnvVars: slices.Clone(p.envVars)})
	}
	return out
}

// AutoModelFallbackError is returned when auto model selection fails because
// no model could be initialized (no API keys configured and no usable Docker
// Model Runner model, e.g. DMR not installed or the pull was declined).
type AutoModelFallbackError struct {
	// Cause is the underlying provider-initialization error, when available
	// (for example "model pull declined by user"). It is surfaced in the
	// message so the user understands why selection fell through, and exposed
	// via Unwrap for errors.Is/As callers.
	Cause error
}

// pullErrorSummarizer is implemented by provider errors (e.g. DMR pull
// failures) that already carry their own multi-line actionable guidance. When
// such an error is the Cause, AutoModelFallbackError shows only its concise
// summary so the two "To fix this" blocks don't stack.
type pullErrorSummarizer interface {
	ModelPullErrorSummary() string
}

func (e *AutoModelFallbackError) Error() string {
	var hints []string
	for _, p := range cloudProviders {
		hint := p.hint
		if p.name == "chatgpt" {
			hint = fmt.Sprintf("sign in with your ChatGPT account: `%s setup`", invocation.DockerAgent())
		}
		hints = append(hints, fmt.Sprintf("    - %s: %s", p.name, hint))
	}

	var b strings.Builder
	if e.Cause != nil {
		var summarizer pullErrorSummarizer
		if errors.As(e.Cause, &summarizer) {
			fmt.Fprintf(&b, "Could not initialize the auto-selected model: %s\n\n", summarizer.ModelPullErrorSummary())
		} else {
			fmt.Fprintf(&b, "Could not initialize the auto-selected model: %v\n\n", e.Cause)
		}
	}
	b.WriteString("No model is currently available.\n\nTo fix this, you can:\n")
	fmt.Fprintf(&b, "  - Run `%s setup` to configure a provider API key or a local model interactively\n", invocation.DockerAgent())
	b.WriteString("  - Pull a Docker Model Runner model, e.g. `docker model pull ai/qwen3`\n")
	b.WriteString("  - Install Docker Model Runner: https://docs.docker.com/ai/model-runner/get-started/\n")
	b.WriteString("  - Configure an API key for a cloud provider:\n")
	b.WriteString(strings.Join(hints, "\n"))
	fmt.Fprintf(&b, "\n\nStep-by-step model setup (API key or local): %s", environment.ModelSetupDocsURL)
	return b.String()
}

// Unwrap exposes the underlying initialization error so callers can inspect it
// with errors.Is/errors.As.
func (e *AutoModelFallbackError) Unwrap() error { return e.Cause }

var DefaultModels = map[string]string{
	"openai":         "gpt-5.6",
	"chatgpt":        "gpt-5.6",
	"github-copilot": "gpt-5.6",
	"anthropic":      "claude-sonnet-4-6",
	"google":         "gemini-3.5-flash",
	"dmr":            "ai/qwen3:latest",
	"mistral":        "mistral-small-latest",
	"openrouter":     "meta-llama/llama-3.3-70b-instruct",
	"baseten":        "deepseek-ai/DeepSeek-V3.1",
	"ovhcloud":       "Qwen3.5-397B-A17B",
	"groq":           "llama-3.3-70b-versatile",
	"fireworks":      "accounts/fireworks/models/kimi-k2-instruct",
	"deepseek":       "deepseek-chat",
	"cerebras":       "gpt-oss-120b",
	"together":       "meta-llama/Llama-3.3-70B-Instruct-Turbo",
	"huggingface":    "meta-llama/Llama-3.3-70B-Instruct",
	"moonshot":       "kimi-k2-0905-preview",
	"vercel":         "openai/gpt-5.6-sol",
	"amazon-bedrock": "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"opencode-go":    "deepseek-v4-flash",
	"opencode-zen":   "deepseek-v4-flash-free",
}

// nonForwardableTokenEnvVars lists provider token env vars that are NOT safe to
// forward as model credentials into an isolated environment (e.g. an eval
// container), even though a provider alias uses them for auth. GITHUB_TOKEN is
// a broad, general-purpose GitHub credential (git, gh, CI, packages) that the
// github-copilot alias happens to reuse; forwarding it would leak far more
// access than a dedicated model API key. CHATGPT_OAUTH_TOKEN is a short-lived
// OAuth access token bound to a ChatGPT subscription, not an API key.
var nonForwardableTokenEnvVars = map[string]bool{
	"GITHUB_TOKEN":      true,
	chatgpt.TokenEnvVar: true,
}

// ProviderAPIKeyEnvVars returns the deduplicated, sorted set of environment
// variables that hold a dedicated model-provider API key. Callers that need to
// forward provider credentials (e.g. into a container) should use this instead
// of hard-coding a list of API key names, so it stays in sync as providers are
// added.
//
// It only includes single-secret API keys: it deliberately excludes non-secret
// detection/mode flags (e.g. GOOGLE_GENAI_USE_VERTEXAI), multi-variable
// credential sets that cannot be forwarded as one secret (e.g. AWS/Bedrock),
// and broad general-purpose tokens (see nonForwardableTokenEnvVars). Providers
// needing those must be given credentials explicitly.
func ProviderAPIKeyEnvVars() []string {
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !nonForwardableTokenEnvVars[name] {
			seen[name] = true
		}
	}
	for _, p := range cloudProviders {
		add(p.apiKeyEnvVar)
	}
	for _, alias := range provider.EachAlias() {
		add(alias.TokenEnvVar)
	}
	return slices.Sorted(maps.Keys(seen))
}

func AvailableProviders(ctx context.Context, modelsGateway string, env environment.Provider) []string {
	if modelsGateway != "" {
		// Default to anthropic when using a gateway
		return []string{"anthropic"}
	}

	var providers []string

	for _, p := range cloudProviders {
		for _, envVar := range p.envVars {
			if key, _ := env.Get(ctx, envVar); key != "" {
				providers = append(providers, p.name)
				break // found one, no need to check other env vars for this provider
			}
		}
	}

	// DMR is always the final fallback
	providers = append(providers, "dmr")

	return providers
}

func AutoModelConfig(ctx context.Context, modelsGateway string, env environment.Provider, defaultModel *latest.ModelConfig, dmrLister DMRModelLister) latest.ModelConfig {
	// If user specified a default model config, use it (with defaults for unset fields)
	if defaultModel != nil && defaultModel.Provider != "" && defaultModel.Model != "" {
		result := *defaultModel
		if result.MaxTokens == nil {
			result.MaxTokens = PreferredMaxTokens(result.Provider)
		}
		return result
	}

	availableProviders := AvailableProviders(ctx, modelsGateway, env)
	firstAvailable := availableProviders[0]

	model := DefaultModels[firstAvailable]
	if firstAvailable == "dmr" {
		// Prefer a model the user already pulled so that, when DMR is set up
		// with models other than ai/qwen3:latest, auto-selection doesn't force
		// a pull prompt and then fail when it's declined.
		model, _ = PickDMRModel(ctx, model, dmrLister)
	}

	return latest.ModelConfig{
		Provider:  firstAvailable,
		Model:     model,
		MaxTokens: PreferredMaxTokens(firstAvailable),
	}
}

// PickDMRModel chooses which Docker Model Runner model a fallback selection
// (the `auto` model or a `first_available` candidate) should use. It prefers
// the configured default when it is already pulled locally; otherwise it falls
// back to the first locally-available (non-embedding) model. When discovery
// fails, finds nothing, or no lister is provided, it returns defaultModel
// unchanged, preserving the behavior of pulling the default on demand.
//
// foundLocal reports whether a locally-available model was chosen. It is false
// when defaultModel was kept because no usable local model exists, so callers
// know an on-demand pull would be required.
func PickDMRModel(ctx context.Context, defaultModel string, lister DMRModelLister) (model string, foundLocal bool) {
	if lister == nil {
		return defaultModel, false
	}

	installed, err := lister(ctx)
	if err != nil {
		slog.DebugContext(ctx, "DMR model discovery failed during selection, using default", "error", err, "default", defaultModel)
		return defaultModel, false
	}
	if len(installed) == 0 {
		return defaultModel, false
	}

	// The default is already pulled: use it so behavior is unchanged for users
	// who do have ai/qwen3:latest.
	if slices.Contains(installed, defaultModel) {
		return defaultModel, true
	}

	// The default model pulled under a different tag (e.g. ai/qwen3:Q4_K_M)
	// still satisfies "prefer the default", so match on the repository.
	defaultRepo := dmrModelRepo(defaultModel)
	for _, m := range installed {
		if dmrModelRepo(m) == defaultRepo {
			slog.DebugContext(ctx, "DMR selection using default model under a non-default tag", "model", m, "default", defaultModel)
			return m, true
		}
	}

	// installed is sorted by the lister; pick the first chat-capable model so
	// the choice is deterministic and never lands on an embedding model.
	for _, m := range installed {
		if !looksLikeEmbeddingModel(m) {
			slog.DebugContext(ctx, "DMR selection using locally-available model", "model", m, "default_not_installed", defaultModel)
			return m, true
		}
	}

	return defaultModel, false
}

// PreferLocalDMRModels adjusts `first_available` selectors that resolved to a
// Docker Model Runner model so they use a locally-pulled model instead of
// forcing an on-demand pull of the configured default. selectorNames is the set
// of model names that were `first_available` selectors before resolution (their
// definitions in cfg are now concrete). cfg is mutated in place.
//
// It returns the set of selector names that resolved to DMR but found no usable
// local model, so callers can treat an initialization failure for those as a
// "no model available" fallback rather than an opaque pull error.
func PreferLocalDMRModels(ctx context.Context, cfg *latest.Config, selectorNames map[string]bool, lister DMRModelLister) map[string]bool {
	noLocal := map[string]bool{}
	for name := range selectorNames {
		mc, ok := cfg.Models[name]
		if !ok || mc.Provider != "dmr" || mc.Model == "" {
			continue
		}
		chosen, foundLocal := PickDMRModel(ctx, mc.Model, lister)
		if chosen != mc.Model {
			mc.Model = chosen
			cfg.Models[name] = mc
		}
		if !foundLocal {
			noLocal[name] = true
		}
	}
	return noLocal
}

// dmrModelRepo returns the repository portion of a DMR model ID, dropping a
// trailing ":<tag>" suffix (e.g. both "ai/qwen3:latest" and "ai/qwen3:Q4_K_M"
// yield "ai/qwen3"). A trailing colon is only treated as a tag separator when
// the suffix has no slash, so a registry host:port like "registry:5000/ai/x"
// is preserved.
func dmrModelRepo(id string) string {
	if i := strings.LastIndex(id, ":"); i >= 0 && !strings.Contains(id[i+1:], "/") {
		return id[:i]
	}
	return id
}

// looksLikeEmbeddingModel reports whether a DMR model ID names an embedding
// model, which should never be chosen as an agent's chat model. It is a simple
// name-substring heuristic (e.g. "ai/embeddinggemma"); the model picker layer
// applies a richer models.dev-backed check for display purposes.
func looksLikeEmbeddingModel(modelID string) bool {
	return strings.Contains(strings.ToLower(modelID), "embed")
}

func PreferredMaxTokens(providerName string) *int64 {
	var mt int64 = 32000
	if providerName == "dmr" {
		mt = 16000
	}
	return &mt
}

// AutoEmbeddingModelConfigs returns the ordered list of embedding-capable models
// to try when a RAG strategy uses `model: auto` for embeddings.
//
// The priority is:
//  1. OpenAI -> text-embedding-3-small model
//  2. DMR -> Google's embeddinggemma model (via Docker Model Runner)
func AutoEmbeddingModelConfigs() []latest.ModelConfig {
	return []latest.ModelConfig{
		{
			Provider: "openai",
			Model:    "text-embedding-3-small",
		},
		{
			Provider: "dmr",
			Model:    "ai/embeddinggemma",
		},
	}
}
