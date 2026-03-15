package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/gateway"
	"github.com/docker/docker-agent/pkg/model/provider"
)

// gatherMissingEnvVars finds out which environment variables are required by the models and tools.
// It returns the missing variables and any non-fatal error encountered during tool discovery.
func gatherMissingEnvVars(ctx context.Context, cfg *latest.Config, modelsGateway string, env environment.Provider) (missing []string, toolErr error) {
	requiredEnv := map[string]bool{}

	// Models
	if modelsGateway == "" {
		names := GatherEnvVarsForModels(cfg)
		for _, e := range names {
			requiredEnv[e] = true
		}
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
		}
	}

	return missing, toolErr
}

func GatherEnvVarsForModels(cfg *latest.Config) []string {
	requiredEnv := map[string]bool{}

	// Inspect only the models that are actually used by agents
	for _, agent := range cfg.Agents {
		modelNames := strings.SplitSeq(agent.Model, ",")
		for modelName := range modelNames {
			modelName = strings.TrimSpace(modelName)
			gatherEnvVarsForModel(cfg, modelName, requiredEnv)
		}
	}

	return sortedKeys(requiredEnv)
}

// gatherEnvVarsForModel collects required environment variables for a single model,
// including any models referenced in its routing rules.
func gatherEnvVarsForModel(cfg *latest.Config, modelName string, requiredEnv map[string]bool) {
	model := cfg.Models[modelName]

	// Add env vars for the model itself
	addEnvVarsForModelConfig(&model, cfg.Providers, requiredEnv)

	// If the model has routing rules, also check all referenced models
	for _, rule := range model.Routing {
		ruleModelName := rule.Model
		if ruleModel, exists := cfg.Models[ruleModelName]; exists {
			// Model reference - add its env vars
			addEnvVarsForModelConfig(&ruleModel, cfg.Providers, requiredEnv)
		} else if providerName, _, ok := strings.Cut(ruleModelName, "/"); ok {
			// Inline spec (e.g., "openai/gpt-4o") - infer env vars from provider
			inlineModel := latest.ModelConfig{Provider: providerName}
			addEnvVarsForModelConfig(&inlineModel, cfg.Providers, requiredEnv)
		}
	}
}

// addEnvVarsForModelConfig adds required environment variables for a model config.
// It checks custom providers first, then built-in aliases, then hardcoded fallbacks.
func addEnvVarsForModelConfig(model *latest.ModelConfig, customProviders map[string]latest.ProviderConfig, requiredEnv map[string]bool) {
	if model.TokenKey != "" {
		requiredEnv[model.TokenKey] = true
	} else if customProviders != nil {
		// Check custom providers from config
		if provCfg, exists := customProviders[model.Provider]; exists {
			if provCfg.TokenKey != "" {
				requiredEnv[provCfg.TokenKey] = true
			}
		}
	} else if alias, exists := provider.Aliases[model.Provider]; exists {
		// Check built-in aliases
		if alias.TokenEnvVar != "" {
			requiredEnv[alias.TokenEnvVar] = true
		}
	} else {
		// Fallback to hardcoded mappings for core providers
		switch model.Provider {
		case "openai":
			requiredEnv["OPENAI_API_KEY"] = true
		case "anthropic":
			requiredEnv["ANTHROPIC_API_KEY"] = true
		case "google":
			if model.ProviderOpts["project"] == nil && model.ProviderOpts["location"] == nil {
				if os.Getenv("GOOGLE_GENAI_USE_VERTEXAI") != "" {
					requiredEnv["GOOGLE_CLOUD_PROJECT"] = true
					requiredEnv["GOOGLE_CLOUD_LOCATION"] = true
				} else if _, exist := os.LookupEnv("GEMINI_API_KEY"); !exist {
					requiredEnv["GOOGLE_API_KEY"] = true
				}
			}
		}
	}

	// Gather env vars from headers (model-level and provider-level) and base URLs
	gatherEnvVarsFromHeaders(model.Headers, requiredEnv)
	gatherEnvVarsFromString(model.BaseURL, requiredEnv)
	if customProviders != nil {
		if provCfg, exists := customProviders[model.Provider]; exists {
			gatherEnvVarsFromHeaders(provCfg.Headers, requiredEnv)
			gatherEnvVarsFromString(provCfg.BaseURL, requiredEnv)
		}
	}
}

// envVarPattern matches ${VAR} and $VAR references in strings.
var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// gatherEnvVarsFromHeaders extracts environment variable names referenced in header values.
func gatherEnvVarsFromHeaders(headers map[string]string, requiredEnv map[string]bool) {
	for _, value := range headers {
		gatherEnvVarsFromString(value, requiredEnv)
	}
}

// gatherEnvVarsFromString extracts environment variable names from a string containing $VAR or ${VAR}.
func gatherEnvVarsFromString(s string, requiredEnv map[string]bool) {
	for _, match := range envVarPattern.FindAllStringSubmatch(s, -1) {
		if match[1] != "" {
			requiredEnv[match[1]] = true
		} else if match[2] != "" {
			requiredEnv[match[2]] = true
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

func sortedKeys(requiredEnv map[string]bool) []string {
	return slices.Sorted(maps.Keys(requiredEnv))
}
