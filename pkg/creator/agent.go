// Package creator provides functionality to create agent configurations interactively.
// It generates a special agent that helps users build their own agent YAML files.
package creator

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/docker/cagent/pkg/config"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/team"
	"github.com/docker/cagent/pkg/teamloader"
	"github.com/docker/cagent/pkg/tools"
	"github.com/docker/cagent/pkg/tools/builtin"
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

	registry := createToolsetRegistry(runConfig.WorkingDir)

	return teamloader.Load(
		ctx,
		config.NewBytesSource("creator", configYAML),
		runConfig,
		teamloader.WithModelOverrides([]string{modelNameOverride}),
		teamloader.WithToolsetRegistry(registry),
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

// createToolsetRegistry creates a custom toolset registry that wraps the filesystem
// toolset to track file paths written by the agent.
func createToolsetRegistry(workingDir string) *teamloader.ToolsetRegistry {
	tracker := &fileWriteTracker{
		ToolSet: builtin.NewFilesystemTool(workingDir),
	}

	registry := teamloader.NewDefaultToolsetRegistry()
	registry.Register("filesystem", func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
		return tracker, nil
	})

	return registry
}

// fileWriteTracker wraps a filesystem toolset to track files written by the agent.
// This allows the creator to know what files were created during the session.
type fileWriteTracker struct {
	tools.ToolSet
	originalWriteFileHandler tools.ToolHandler
	path                     string
}

// Tools returns the available tools, wrapping the write_file tool to track paths.
func (t *fileWriteTracker) Tools(ctx context.Context) ([]tools.Tool, error) {
	innerTools, err := t.ToolSet.Tools(ctx)
	if err != nil {
		return nil, err
	}

	for i, tool := range innerTools {
		if tool.Name == builtin.ToolNameWriteFile {
			t.originalWriteFileHandler = tool.Handler
			innerTools[i].Handler = t.trackWriteFile
		}
	}

	return innerTools, nil
}

// trackWriteFile intercepts write_file calls to track the path being written.
func (t *fileWriteTracker) trackWriteFile(ctx context.Context, toolCall tools.ToolCall) (*tools.ToolCallResult, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
		return nil, fmt.Errorf("failed to parse write_file arguments: %w", err)
	}

	t.path = args.Path

	return t.originalWriteFileHandler(ctx, toolCall)
}

// LastWrittenPath returns the path of the last file written by the agent.
// Returns an empty string if no file has been written yet.
func (t *fileWriteTracker) LastWrittenPath() string {
	return t.path
}
