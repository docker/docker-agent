// Package creator provides functionality to create agent configurations interactively.
// It generates a special agent that helps users build their own agent YAML files.
package creator

import (
	"context"
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/cli/invocation"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
	loaderdefaults "github.com/docker/docker-agent/pkg/teamloader/defaults"
)

//go:embed instructions.txt
var agentBuilderInstructions string

// Constants for the creator agent configuration.
const (
	creatorAgentName      = "root"
	creatorAgentModel     = "auto"
	creatorWelcomeMessage = "Hello! I'm here to create agents for you.\n\nCan you explain to me what the agent will be used for?"
)

// Load creates and returns the agent-builder team load result.
// The agent builder helps users create their own agent configurations interactively.
//
// Parameters:
//   - ctx: Context for the operation
//   - runConfig: Runtime configuration including working directory and environment
//   - modelNameOverride: Optional model override (empty string uses auto-selection)
//
// Returns the configured team or an error if configuration fails.
func Load(ctx context.Context, runConfig *config.RuntimeConfig, modelNameOverride string) (*teamloader.LoadResult, error) {
	instructions := buildInstructions(ctx, runConfig, modelNameOverride)

	configYAML, err := buildCreatorConfigYAML(instructions)
	if err != nil {
		return nil, fmt.Errorf("building creator config: %w", err)
	}

	return teamloader.LoadWithConfig(
		ctx,
		config.NewBytesSource("creator", configYAML),
		runConfig,
		append(loaderdefaults.Opts(), teamloader.WithModelOverrides([]string{modelNameOverride}))...,
	)
}

// Agent creates and returns a team configured for the agent builder functionality.
func Agent(ctx context.Context, runConfig *config.RuntimeConfig, modelNameOverride string) (*team.Team, error) {
	result, err := Load(ctx, runConfig, modelNameOverride)
	if err != nil {
		return nil, err
	}
	return result.Team, nil
}

// buildInstructions creates the full instruction set for the creator agent,
// including provider-specific model configuration examples.
func buildInstructions(ctx context.Context, runConfig *config.RuntimeConfig, modelNameOverride string) string {
	usableProviders := config.AvailableProviders(ctx, runConfig.ModelsGateway, runConfig.EnvProvider())

	var b strings.Builder
	b.WriteString(strings.ReplaceAll(agentBuilderInstructions, "{{docker_agent_command}}", invocation.DockerAgent()))
	b.WriteString("\n\nPreferred model providers to use: ")
	b.WriteString(strings.Join(usableProviders, ", "))
	b.WriteString(". You must always use one or more of the following model configurations: \n")

	for _, provider := range usableProviders {
		model := config.DefaultModels[provider]
		maxTokens := config.PreferredMaxTokens(provider)
		fmt.Fprintf(&b, `
		models:
			%s:
				provider: %s
				model: %s
				max_tokens: %d
`, provider, provider, model, *maxTokens)
	}

	appendCustomProviderInstructions(ctx, &b, runConfig, modelNameOverride)

	return b.String()
}

// appendCustomProviderInstructions documents the user-defined custom providers
// (from the user config) that have usable credentials, so the generated agent
// can target them. Each one is emitted with its `providers` section so the
// generated YAML stays self-contained and portable.
func appendCustomProviderInstructions(ctx context.Context, b *strings.Builder, runConfig *config.RuntimeConfig, modelNameOverride string) {
	env := runConfig.EnvProvider()

	var names []string
	for name, provCfg := range runConfig.Providers {
		if provCfg.TokenKey != "" {
			if token, _ := env.Get(ctx, provCfg.TokenKey); token == "" {
				continue
			}
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)

	b.WriteString("\nThe user also defined the following custom providers. When one is used, copy its `providers` entry verbatim into the generated YAML and reference it from a model definition:\n")

	for _, name := range names {
		provCfg := runConfig.Providers[name]

		model := "REPLACE_WITH_MODEL_ID"
		if parsed, err := latest.ParseModelRef(modelNameOverride); err == nil && parsed.Provider == name {
			model = parsed.Model
		}

		fmt.Fprintf(b, "\n\t\tproviders:\n\t\t\t%s:\n", name)
		if provCfg.Provider != "" {
			fmt.Fprintf(b, "\t\t\t\tprovider: %s\n", provCfg.Provider)
		}
		if provCfg.BaseURL != "" {
			fmt.Fprintf(b, "\t\t\t\tbase_url: %s\n", provCfg.BaseURL)
		}
		if provCfg.APIType != "" {
			fmt.Fprintf(b, "\t\t\t\tapi_type: %s\n", provCfg.APIType)
		}
		if provCfg.TokenKey != "" {
			fmt.Fprintf(b, "\t\t\t\ttoken_key: %s\n", provCfg.TokenKey)
		}
		fmt.Fprintf(b, "\t\tmodels:\n\t\t\t%s:\n\t\t\t\tprovider: %s\n\t\t\t\tmodel: %s\n", name, name, model)
		if model == "REPLACE_WITH_MODEL_ID" {
			fmt.Fprintf(b, "\t\tAsk the user which model ID to use with %q (they can list them with `%s models --provider %s`).\n", name, invocation.DockerAgent(), name)
		}
	}
}

// buildCreatorConfigYAML generates the YAML configuration for the creator agent.
// It uses yaml.MapSlice to ensure proper indentation of multi-line strings.
func buildCreatorConfigYAML(instructions string) ([]byte, error) {
	// Define available toolsets for the creator agent
	toolsets := []map[string]any{
		{"type": "shell"},
		{"type": "filesystem"},
	}

	// Build the root agent configuration
	rootAgent := yaml.MapSlice{
		{Key: "model", Value: creatorAgentModel},
		{Key: "welcome_message", Value: creatorWelcomeMessage},
		{Key: "instruction", Value: instructions},
		{Key: "toolsets", Value: toolsets},
	}

	// Build the full config structure
	agentsConfig := yaml.MapSlice{
		{Key: creatorAgentName, Value: rootAgent},
	}

	fullConfig := yaml.MapSlice{
		{Key: "agents", Value: agentsConfig},
	}

	return yaml.Marshal(fullConfig)
}
