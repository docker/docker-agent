package config

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
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
}

// cloudProviders defines the available cloud providers in priority order.
// The first provider with a configured API key will be selected by AutoModelConfig.
// DMR is always appended as the final fallback (not listed here).
var cloudProviders = []providerConfig{
	{"anthropic", []string{"ANTHROPIC_API_KEY"}, "ANTHROPIC_API_KEY"},
	{"openai", []string{"OPENAI_API_KEY"}, "OPENAI_API_KEY"},
	{"google", []string{
		"GOOGLE_API_KEY",
		"GEMINI_API_KEY",
		"GOOGLE_GENAI_USE_VERTEXAI",
	}, "GOOGLE_API_KEY (or GEMINI_API_KEY, GOOGLE_GENAI_USE_VERTEXAI)"},
	{"mistral", []string{"MISTRAL_API_KEY"}, "MISTRAL_API_KEY"},
	{"amazon-bedrock", []string{
		"AWS_BEARER_TOKEN_BEDROCK",
		"AWS_ACCESS_KEY_ID",
		"AWS_PROFILE",
		"AWS_ROLE_ARN",
	}, "AWS_ACCESS_KEY_ID (or AWS_PROFILE, AWS_ROLE_ARN, AWS_BEARER_TOKEN_BEDROCK)"},
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

func (e *AutoModelFallbackError) Error() string {
	var hints []string
	for _, p := range cloudProviders {
		hints = append(hints, fmt.Sprintf("    - %s: %s", p.name, p.hint))
	}

	var b strings.Builder
	if e.Cause != nil {
		fmt.Fprintf(&b, "Could not initialize the auto-selected model: %v\n\n", e.Cause)
	}
	b.WriteString("No model is currently available.\n\nTo fix this, you can:\n")
	b.WriteString("  - Pull a Docker Model Runner model, e.g. `docker model pull ai/qwen3`\n")
	b.WriteString("  - Install Docker Model Runner: https://docs.docker.com/ai/model-runner/get-started/\n")
	b.WriteString("  - Configure an API key for a cloud provider:\n")
	b.WriteString(strings.Join(hints, "\n"))
	return b.String()
}

// Unwrap exposes the underlying initialization error so callers can inspect it
// with errors.Is/errors.As.
func (e *AutoModelFallbackError) Unwrap() error { return e.Cause }

var DefaultModels = map[string]string{
	"openai":         "gpt-5",
	"anthropic":      "claude-sonnet-4-6",
	"google":         "gemini-3.5-flash",
	"dmr":            "ai/qwen3:latest",
	"mistral":        "mistral-small-latest",
	"amazon-bedrock": "global.anthropic.claude-sonnet-4-5-20250929-v1:0",
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
		model = pickDMRAutoModel(ctx, model, dmrLister)
	}

	return latest.ModelConfig{
		Provider:  firstAvailable,
		Model:     model,
		MaxTokens: PreferredMaxTokens(firstAvailable),
	}
}

// pickDMRAutoModel chooses which Docker Model Runner model auto-selection
// should use. It prefers the configured default when it is already pulled
// locally; otherwise it falls back to the first locally-available
// (non-embedding) model. When discovery fails, finds nothing, or no lister is
// provided, it returns defaultModel unchanged, preserving the previous
// behavior of pulling the default on demand.
func pickDMRAutoModel(ctx context.Context, defaultModel string, lister DMRModelLister) string {
	if lister == nil {
		return defaultModel
	}

	installed, err := lister(ctx)
	if err != nil {
		slog.DebugContext(ctx, "DMR model discovery failed during auto-selection, using default", "error", err, "default", defaultModel)
		return defaultModel
	}
	if len(installed) == 0 {
		return defaultModel
	}

	// The default is already pulled: use it so behavior is unchanged for users
	// who do have ai/qwen3:latest.
	if slices.Contains(installed, defaultModel) {
		return defaultModel
	}

	// The default model pulled under a different tag (e.g. ai/qwen3:Q4_K_M)
	// still satisfies "prefer the default", so match on the repository.
	defaultRepo := dmrModelRepo(defaultModel)
	for _, m := range installed {
		if dmrModelRepo(m) == defaultRepo {
			slog.DebugContext(ctx, "DMR auto-selection using default model under a non-default tag", "model", m, "default", defaultModel)
			return m
		}
	}

	// installed is sorted by the lister; pick the first chat-capable model so
	// the choice is deterministic and never lands on an embedding model.
	for _, m := range installed {
		if !looksLikeEmbeddingModel(m) {
			slog.DebugContext(ctx, "DMR auto-selection using locally-available model", "model", m, "default_not_installed", defaultModel)
			return m
		}
	}

	return defaultModel
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

func PreferredMaxTokens(provider string) *int64 {
	var mt int64 = 32000
	if provider == "dmr" {
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
