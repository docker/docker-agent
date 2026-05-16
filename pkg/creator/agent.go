// Package creator provides functionality to create agent configurations interactively.
// It generates a special agent that helps users build their own agent YAML files.
package creator

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/teamloader"
)

//go:embed instructions.txt
var agentBuilderInstructions string

// Constants for the creator agent configuration.
const (
	creatorAgentName      = "root"
	creatorAgentModel     = "auto"
	creatorWelcomeMessage = "Hello! I'm here to create agents for you.\n\nCan you explain to me what the agent will be used for?"
)

// Agent creates and returns a team configured for the agent builder functionality.
// The agent builder helps users create their own agent configurations interactively.
//
// Parameters:
//   - ctx: Context for the operation
//   - runConfig: Runtime configuration including working directory and environment
//   - modelNameOverride: Optional model override (empty string uses auto-selection)
//
// Returns the configured team or an error if configuration fails.
func Agent(ctx context.Context, runConfig *config.RuntimeConfig, modelNameOverride string) (*team.Team, error) {
	instructions := buildInstructions(ctx, runConfig)

	configYAML, err := buildCreatorConfigYAML(instructions)
	if err != nil {
		return nil, fmt.Errorf("building creator config: %w", err)
	}

	return teamloader.Load(
		ctx,
		config.NewBytesSource("creator", configYAML),
		runConfig,
		teamloader.WithModelOverrides([]string{modelNameOverride}),
	)
}

// buildInstructions creates the full instruction set for the creator agent,
// including provider-specific model configuration examples.
func buildInstructions(ctx context.Context, runConfig *config.RuntimeConfig) string {
	usableProviders := config.AvailableProviders(ctx, runConfig.ModelsGateway, runConfig.EnvProvider())

	var b strings.Builder
	b.WriteString(agentBuilderInstructions)
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

	return b.String()
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
