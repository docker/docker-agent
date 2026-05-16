package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"

	hclconv "github.com/docker/docker-agent/pkg/config/hcl"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
)

func Load(ctx context.Context, source Source) (*latest.Config, error) {
	data, err := source.Read(ctx)
	if err != nil {
		return nil, err
	}

	// Configurations may be authored in HCL as an alternative to YAML.
	// Detect the format from the source name extension or, when no hint is
	// available (OCI artifacts, etc.), from the content itself, then
	// transparently convert to YAML for the rest of the pipeline.
	if isHCLSource(source.Name(), data) {
		data, err = hclconv.ToYAML(data, source.Name())
		if err != nil {
			return nil, fmt.Errorf("parsing HCL config file: %w", err)
		}
	}

	var raw struct {
		Version string `yaml:"version,omitempty"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("looking for version in config file\n%s", yaml.FormatError(err, true, true))
	}
	raw.Version = cmp.Or(raw.Version, latest.Version)

	oldConfig, err := parseCurrentVersion(data, raw.Version)
	if err != nil {
		return nil, fmt.Errorf("parsing config file\n%s", yaml.FormatError(err, true, true))
	}

	config, err := migrateToLatestConfig(oldConfig, data)
	if err != nil {
		return nil, fmt.Errorf("migrating config: %w", err)
	}

	config.Version = raw.Version

	if err := validateConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// CheckRequiredEnvVars checks which environment variables are required by the models and tools.
//
// This allows exiting early with a proper error message instead of failing later when trying to use a model or tool.
func CheckRequiredEnvVars(ctx context.Context, cfg *latest.Config, modelsGateway string, env environment.Provider) error {
	if modelsGateway != "" {
		if jwt, _ := env.Get(ctx, environment.DockerDesktopTokenEnv); jwt == "" {
			return errors.New("sorry, you first need to sign in Docker Desktop to use the Docker AI Gateway")
		}
	}

	missing, err := gatherMissingEnvVars(ctx, cfg, modelsGateway, env)
	if err != nil {
		// If there's a tool preflight error, log it but continue
		slog.WarnContext(ctx, "Failed to preflight toolset environment variables; continuing", "error", err)
	}

	// Return error if there are missing environment variables
	if len(missing) > 0 {
		return &environment.RequiredEnvError{
			Missing: missing,
		}
	}

	return nil
}

func parseCurrentVersion(data []byte, version string) (any, error) {
	parsers, _ := versions()
	parser, found := parsers[version]
	if !found {
		return nil, fmt.Errorf("unsupported config version: %v (valid versions: %s)", version, strings.Join(slices.Sorted(maps.Keys(parsers)), ", "))
	}
	return parser(data)
}

func migrateToLatestConfig(c any, raw []byte) (latest.Config, error) {
	var err error

	_, upgraders := versions()
	for _, upgrade := range upgraders {
		c, err = upgrade(c, raw)
		if err != nil {
			return latest.Config{}, err
		}
	}

	return c.(latest.Config), nil
}

func validateConfig(cfg *latest.Config) error {
	if err := validateProviders(cfg); err != nil {
		return err
	}

	if cfg.Models == nil {
		cfg.Models = map[string]latest.ModelConfig{}
	}

	for name := range cfg.Models {
		if cfg.Models[name].ParallelToolCalls == nil {
			m := cfg.Models[name]
			m.ParallelToolCalls = new(true)
			cfg.Models[name] = m
		}
	}

	if err := ensureModelsExist(cfg); err != nil {
		return err
	}

	if err := resolveMCPDefinitions(cfg); err != nil {
		return err
	}

	if err := resolveRAGDefinitions(cfg); err != nil {
		return err
	}

	allNames := map[string]bool{}
	for _, agent := range cfg.Agents {
		allNames[agent.Name] = true
	}

	for _, agent := range cfg.Agents {
		for _, subAgentRef := range agent.SubAgents {
			if _, exists := allNames[subAgentRef]; !exists && !IsExternalReference(subAgentRef) {
				return fmt.Errorf("agent '%s' references non-existent sub-agent '%s'", agent.Name, subAgentRef)
			}
			if IsExternalReference(subAgentRef) {
				name, _ := ParseExternalAgentRef(subAgentRef)
				if allNames[name] {
					return fmt.Errorf("agent '%s': external sub-agent '%s' resolves to name '%s' which conflicts with a locally-defined agent", agent.Name, subAgentRef, name)
				}
			}
		}

		for _, handoffRef := range agent.Handoffs {
			if _, exists := allNames[handoffRef]; !exists && !IsExternalReference(handoffRef) {
				return fmt.Errorf("agent '%s' references non-existent handoff agent '%s'", agent.Name, handoffRef)
			}
			if IsExternalReference(handoffRef) {
				name, _ := ParseExternalAgentRef(handoffRef)
				if allNames[name] {
					return fmt.Errorf("agent '%s': external handoff '%s' resolves to name '%s' which conflicts with a locally-defined agent", agent.Name, handoffRef, name)
				}
			}
		}

		if err := validateSkillsConfiguration(agent.Name, &agent); err != nil {
			return err
		}
	}

	return nil
}

// isHCLSource reports whether the configuration data should be parsed as HCL
// rather than YAML. The decision is based first on the source name extension,
// and then on a content-based heuristic when no extension hint is available.
func isHCLSource(name string, data []byte) bool {
	if strings.EqualFold(filepath.Ext(name), ".hcl") {
		return true
	}
	return hclconv.LooksLikeHCL(data)
}

// providerAPITypes are the allowed values for api_type in provider configs
var providerAPITypes = map[string]bool{
	"":                       true, // empty is allowed (defaults to openai_chatcompletions)
	"openai_chatcompletions": true,
	"openai_responses":       true,
}

// validateProviders validates all provider configurations
func validateProviders(cfg *latest.Config) error {
	if cfg.Providers == nil {
		return nil
	}

	for name, provCfg := range cfg.Providers {
		// Validate provider name
		if err := validateProviderName(name); err != nil {
			return fmt.Errorf("provider '%s': %w", name, err)
		}

		// Validate api_type if set
		if !providerAPITypes[provCfg.APIType] {
			return fmt.Errorf("provider '%s': invalid api_type '%s' (must be one of: openai_chatcompletions, openai_responses)", name, provCfg.APIType)
		}

		// base_url is required for OpenAI-compatible providers (the default)
		// but optional for native providers like anthropic, google, amazon-bedrock
		if provCfg.BaseURL != "" {
			if _, err := url.Parse(provCfg.BaseURL); err != nil {
				return fmt.Errorf("provider '%s': invalid base_url '%s': %w", name, provCfg.BaseURL, err)
			}
		} else if isOpenAICustomProvider(provCfg) {
			return fmt.Errorf("provider '%s': base_url is required for OpenAI-compatible providers", name)
		}

		// token_key is optional - if not set, requests will be sent without bearer token
	}

	return nil
}

// isOpenAICustomProvider returns true if the provider config describes an OpenAI-compatible
// custom provider (i.e., Provider is empty or "openai", or api_type is explicitly set to an
// OpenAI schema). These providers require a base_url because they don't have a built-in default.
func isOpenAICustomProvider(cfg latest.ProviderConfig) bool {
	// If api_type is explicitly set, it's an OpenAI-compatible provider
	if cfg.APIType != "" {
		return true
	}
	// If provider is empty (defaults to openai) or explicitly "openai"
	return cfg.Provider == "" || cfg.Provider == "openai"
}

// validateProviderName validates that a provider name is valid
func validateProviderName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("name cannot be empty")
	}
	if trimmed != name {
		return errors.New("name cannot have leading or trailing whitespace")
	}
	if strings.Contains(name, "/") {
		return errors.New("name cannot contain '/'")
	}
	return nil
}

// validateSkillsConfiguration validates the skills configuration for an agent.
func validateSkillsConfiguration(_ string, agent *latest.AgentConfig) error {
	for _, source := range agent.Skills.Sources {
		switch {
		case source == latest.SkillSourceLocal:
			// valid
		case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
			if _, err := url.Parse(source); err != nil {
				return fmt.Errorf("agent '%s' has invalid skills source URL '%s': %w", agent.Name, source, err)
			}
		default:
			return fmt.Errorf("agent '%s' has unknown skills source '%s' (must be 'local' or an HTTP/HTTPS URL)", agent.Name, source)
		}
	}
	for _, name := range agent.Skills.Include {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("agent '%s' has an empty skills entry", agent.Name)
		}
	}
	return nil
}
