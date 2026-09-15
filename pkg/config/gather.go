package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/gateway"
	"github.com/docker/docker-agent/pkg/model/provider"
)

// gatherMissingEnvVars finds out which environment variables are required by the models and tools.
// It returns the missing variables, whether any of them is a model-provider
// credential (as opposed to a tool secret), and any non-fatal error
// encountered during tool discovery.
func gatherMissingEnvVars(ctx context.Context, cfg *latest.Config, modelsGateway string, env environment.Provider) (missing []string, missingModelCreds bool, toolErr error) {
	requiredEnv := map[string]bool{}
	modelEnv := map[string]bool{}

	// Models
	var modelNames []string
	if modelsGateway == "" {
		modelNames = GatherEnvVarsForModels(ctx, cfg, env)
	} else {
		// A gateway supplies credentials for routed models, but models that
		// bypass it dial their provider directly and still need their own
		// credentials present.
		modelNames = gatherEnvVarsForModels(ctx, cfg, env, true)
	}
	for _, e := range modelNames {
		requiredEnv[e] = true
		modelEnv[e] = true
	}

	// Tools
	names, err := GatherEnvVarsForTools(ctx, cfg)
	if err != nil {
		// Store tool preflight error but continue checking models
		toolErr = err
	}
	// Always add tool env vars, even when some toolsets had preflight errors.
	// Previously, a preflight error from one toolset would cause all tool
	// env vars to be silently skipped.
	for _, e := range names {
		requiredEnv[e] = true
	}

	for _, e := range sortedKeys(requiredEnv) {
		if v, _ := env.Get(ctx, e); v == "" {
			missing = append(missing, e)
			missingModelCreds = missingModelCreds || modelEnv[e]
		}
	}

	return missing, missingModelCreds, toolErr
}

func GatherEnvVarsForModels(ctx context.Context, cfg *latest.Config, env environment.Provider) []string {
	return gatherEnvVarsForModels(ctx, cfg, env, false)
}

// RequiredModelEnvVars returns the env vars the config's models need under
// the given gateway configuration, mirroring the run-time preflight check:
// every model credential when no models gateway is set, and only those of
// models that bypass the gateway otherwise (the gateway supplies credentials
// for routed models).
func RequiredModelEnvVars(ctx context.Context, cfg *latest.Config, modelsGateway string, env environment.Provider) []string {
	return gatherEnvVarsForModels(ctx, cfg, env, modelsGateway != "")
}

// gatherEnvVarsForModels collects the env vars required by model-backed agents.
// When bypassOnly is true, only the leaf models that effectively dial their
// provider directly (bypassing the models gateway) are inspected — used to
// require direct provider credentials for those models even when a models
// gateway would otherwise supply credentials for the rest.
func gatherEnvVarsForModels(ctx context.Context, cfg *latest.Config, env environment.Provider, bypassOnly bool) []string {
	requiredEnv := map[string]bool{}

	// Inspect only the models that are actually used by docker-agent model-backed agents.
	for _, agent := range cfg.Agents {
		if agent.Harness != nil {
			continue
		}
		modelNames := strings.SplitSeq(agent.Model, ",")
		for modelName := range modelNames {
			modelName = strings.TrimSpace(modelName)
			gatherEnvVarsForModel(ctx, cfg, modelName, requiredEnv, env, bypassOnly)
		}
		gatherEnvVarsForCompactionModel(ctx, cfg, &agent, requiredEnv, env, bypassOnly)
	}

	return sortedKeys(requiredEnv)
}

// gatherEnvVarsForCompactionModel collects the env vars required by the
// agent's effective compaction model (agent > model > provider precedence,
// see [EffectiveCompactionModelRef]), so its credentials surface in the same
// consolidated preflight error as the primary model's. A named reference goes
// through the regular per-model path (which also covers routing rules); an
// inline "provider/model" spec is parsed directly. Invalid references are
// ignored here — they fail with a dedicated error when the model is built.
func gatherEnvVarsForCompactionModel(ctx context.Context, cfg *latest.Config, agent *latest.AgentConfig, requiredEnv map[string]bool, env environment.Provider, bypassOnly bool) {
	ref := EffectiveCompactionModelRef(cfg, agent)
	if ref == "" {
		return
	}
	if _, exists := cfg.Models[ref]; exists {
		gatherEnvVarsForModel(ctx, cfg, ref, requiredEnv, env, bypassOnly)
		return
	}
	inline, err := latest.ParseModelRef(ref)
	if err != nil {
		return
	}
	if !bypassOnly || hasCustomBaseURL(&inline, cfg.Providers) {
		addEnvVarsForModelConfig(ctx, &inline, cfg.Providers, requiredEnv, env)
	}
}

// gatherEnvVarsForModel collects required environment variables for a single model,
// including any models referenced in its routing rules.
//
// When bypassOnly is true, a leaf's credentials are collected only when that
// leaf effectively bypasses the gateway. A routing model bypasses its whole
// subtree (the runtime propagates the flag to the fallback and every routed
// target), and a routed named model can additionally opt in on its own.
func gatherEnvVarsForModel(ctx context.Context, cfg *latest.Config, modelName string, requiredEnv map[string]bool, env environment.Provider, bypassOnly bool) {
	model := cfg.Models[modelName]
	rootBypassed := model.BypassModelsGateway

	// The model's own provider/model is a leaf: either the model itself or, for
	// a router, its fallback. The runtime rebuilds a router's fallback from its
	// "provider/model" spec (see rulebased.NewClient), resolving it as a named
	// model when one exists and keeping only provider/model otherwise; mirror
	// that here so router-level base_url/token_key are not misattributed to the
	// fallback. A custom base_url implies the bypass: such endpoints are never
	// routed through the models gateway (see createDirectProvider).
	leaf := model
	if len(model.Routing) > 0 {
		leaf = routerFallbackLeaf(cfg, model)
	}
	if !bypassOnly || rootBypassed || leaf.BypassModelsGateway || hasCustomBaseURL(&leaf, cfg.Providers) {
		addEnvVarsForModelConfig(ctx, &leaf, cfg.Providers, requiredEnv, env)
	}

	// If the model has routing rules, also check all referenced models.
	for _, rule := range model.Routing {
		ruleModelName := rule.Model
		if ruleModel, exists := cfg.Models[ruleModelName]; exists {
			// Named model reference. A routed target bypasses when the router
			// does (propagation), when it sets its own flag, or when it dials a
			// custom base_url (implied bypass).
			if !bypassOnly || rootBypassed || ruleModel.BypassModelsGateway || hasCustomBaseURL(&ruleModel, cfg.Providers) {
				addEnvVarsForModelConfig(ctx, &ruleModel, cfg.Providers, requiredEnv, env)
			}
		} else if providerName, _, ok := strings.Cut(ruleModelName, "/"); ok {
			// Inline spec (e.g., "openai/gpt-4o") - infer env vars from provider.
			// Inline specs carry no flag of their own; they bypass via the
			// router's propagated bypass or a custom provider's base_url.
			inlineModel := latest.ModelConfig{Provider: providerName}
			if !bypassOnly || rootBypassed || hasCustomBaseURL(&inlineModel, cfg.Providers) {
				addEnvVarsForModelConfig(ctx, &inlineModel, cfg.Providers, requiredEnv, env)
			}
		}
	}
}

// routerFallbackLeaf mirrors how the runtime resolves a router's fallback
// (see rulebased.NewClient): the "provider/model" spec is looked up as a
// named model first, otherwise reparsed inline, which keeps only the
// provider and model fields.
func routerFallbackLeaf(cfg *latest.Config, router latest.ModelConfig) latest.ModelConfig {
	spec := router.Provider + "/" + router.Model
	if fallback, exists := cfg.Models[spec]; exists {
		return fallback
	}
	return latest.ModelConfig{Provider: router.Provider, Model: router.Model}
}

// addEnvVarsForModelConfig adds required environment variables for a model config.
// It checks custom providers first, then built-in aliases, then hardcoded fallbacks.
func addEnvVarsForModelConfig(ctx context.Context, model *latest.ModelConfig, customProviders map[string]latest.ProviderConfig, requiredEnv map[string]bool, env environment.Provider) {
	// The model and base_url fields support ${env.X}/${X} substitution, so any
	// variable they reference must be set for the provider to be built (issue
	// #2261). Collect these regardless of the credential logic below, which can
	// return early (e.g. when base_url is set).
	for _, field := range []string{model.Model, model.BaseURL} {
		for _, name := range environment.Refs(field) {
			requiredEnv[name] = true
		}
	}
	// A provider-level base_url is merged into the model at runtime when the
	// model does not override it (see mergeFromProviderConfig), so its ${env.X}
	// references must be resolvable too.
	if model.BaseURL == "" {
		if provCfg, exists := customProviders[model.Provider]; exists {
			for _, name := range environment.Refs(provCfg.BaseURL) {
				requiredEnv[name] = true
			}
		}
	}

	// A model with non-API-key auth (e.g. Workload Identity Federation) does
	// not require a TokenKey or the hardcoded API-key env var. Instead, the
	// env vars referenced by its identity-token source are required.
	if auth := latest.EffectiveAuth(*model, customProviders); auth != nil {
		for _, name := range auth.EnvVars() {
			requiredEnv[name] = true
		}
		return
	}

	if model.TokenKey != "" {
		requiredEnv[model.TokenKey] = true
		return
	}
	if model.BaseURL != "" {
		return
	}
	if customProviders != nil {
		// Check custom providers from config
		if provCfg, exists := customProviders[model.Provider]; exists {
			if provCfg.TokenKey != "" {
				requiredEnv[provCfg.TokenKey] = true
			} else if provCfg.BaseURL == "" {
				// Custom providers with a base_url and no token_key are intentionally
				// unauthenticated; native provider aliases without a base_url use the
				// effective provider's default credentials.
				effective := provCfg.Provider
				if effective == "" {
					effective = "openai"
				}
				addEnvVarsForCoreProvider(ctx, effective, model, requiredEnv, env)
			}
			return
		}
	}
	if alias, exists := provider.LookupAlias(model.Provider); exists {
		// Check built-in aliases
		if alias.TokenEnvVar != "" {
			if model.Provider == "github-copilot" {
				requiredEnv[githubCopilotTokenEnvVar(ctx, env)] = true
			} else {
				requiredEnv[alias.TokenEnvVar] = true
			}
		}
		// A templated alias base URL (e.g. Cloudflare's account/gateway-scoped
		// endpoint) references env vars that must resolve when the provider is
		// built, so surface them in the preflight check too.
		for _, name := range environment.Refs(alias.BaseURL) {
			requiredEnv[name] = true
		}
	} else {
		addEnvVarsForCoreProvider(ctx, model.Provider, model, requiredEnv, env)
	}
}

func githubCopilotTokenEnvVar(ctx context.Context, env environment.Provider) string {
	if value, _ := env.Get(ctx, "GITHUB_TOKEN"); value != "" {
		return "GITHUB_TOKEN"
	}
	if value, _ := env.Get(ctx, "GH_TOKEN"); value != "" {
		return "GH_TOKEN"
	}
	return "GITHUB_TOKEN"
}

// addEnvVarsForCoreProvider adds the required env vars for a core provider type.
func addEnvVarsForCoreProvider(ctx context.Context, providerType string, model *latest.ModelConfig, requiredEnv map[string]bool, env environment.Provider) {
	switch providerType {
	case "openai":
		requiredEnv["OPENAI_API_KEY"] = true
	case "anthropic":
		requiredEnv["ANTHROPIC_API_KEY"] = true
	case "google":
		if model.ProviderOpts["project"] == nil && model.ProviderOpts["location"] == nil {
			if value, _ := env.Get(ctx, "GOOGLE_GENAI_USE_VERTEXAI"); isVertexAIEnabled(value) {
				requiredEnv["GOOGLE_CLOUD_PROJECT"] = true
				requiredEnv["GOOGLE_CLOUD_LOCATION"] = true
			} else if value, _ := env.Get(ctx, "GEMINI_API_KEY"); value == "" {
				requiredEnv["GOOGLE_API_KEY"] = true
			}
		}
	}
}

func GatherEnvVarsForTools(ctx context.Context, cfg *latest.Config) ([]string, error) {
	requiredEnv := map[string]bool{}
	var errs []error

	for i := range cfg.Agents {
		agent := cfg.Agents[i]

		for j := range agent.Toolsets {
			toolSet := agent.Toolsets[j]
			ref := toolSet.Ref
			if toolSet.Type != "mcp" || ref == "" {
				continue
			}

			mcpServerName := gateway.ParseServerRef(ref)
			secrets, err := gateway.RequiredEnvVars(ctx, mcpServerName)
			if err != nil {
				errs = append(errs, fmt.Errorf("reading which secrets the MCP server needs for %s: %w", ref, err))
				continue
			}

			for _, secret := range secrets {
				value, ok := toolSet.Env[secret.Env]
				if !ok {
					requiredEnv[secret.Env] = true
				} else {
					os.Expand(value, func(name string) string {
						requiredEnv[name] = true
						return ""
					})
				}
			}
		}
	}

	if len(errs) > 0 {
		return sortedKeys(requiredEnv), fmt.Errorf("tool env preflight: %w", errors.Join(errs...))
	}
	return sortedKeys(requiredEnv), nil
}

// isVertexAIEnabled interprets GOOGLE_GENAI_USE_VERTEXAI as a boolean,
// mirroring the provider routing in pkg/model/provider/gemini. Only an
// explicit truthy value (per strconv.ParseBool) enables the Vertex AI path,
// so "false", "0" or "" require the direct Gemini API credentials instead.
func isVertexAIEnabled(value string) bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && enabled
}

func sortedKeys(requiredEnv map[string]bool) []string {
	return slices.Sorted(maps.Keys(requiredEnv))
}
