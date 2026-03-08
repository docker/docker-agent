package creator

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/cagent/pkg/config"
	"github.com/docker/cagent/pkg/config/latest"
	"github.com/docker/cagent/pkg/environment"
)

func TestAgentConfigYAML(t *testing.T) {
	t.Parallel()

	// Build the same structure as the buildCreatorConfigYAML function
	agentToolsets := []map[string]any{
		{"type": "shell"},
		{"type": "filesystem"},
	}

	rootAgent := yaml.MapSlice{
		{Key: "model", Value: "auto"},
		{Key: "welcome_message", Value: "Hello! I'm here to create agents for you.\n\nCan you explain to me what the agent will be used for?"},
		{Key: "instruction", Value: "Some test instructions"},
		{Key: "toolsets", Value: agentToolsets},
	}

	agentsMapSlice := yaml.MapSlice{
		{Key: "root", Value: rootAgent},
	}

	newAgentConfig := yaml.MapSlice{
		{Key: "agents", Value: agentsMapSlice},
	}

	data, err := yaml.Marshal(newAgentConfig)
	require.NoError(t, err)

	t.Logf("YAML output:\n%s", string(data))

	// Verify it can be loaded by the config loader
	cfg, err := config.Load(t.Context(), config.NewBytesSource("test", data))
	require.NoError(t, err)

	// Verify the config has the expected structure
	require.Len(t, cfg.Agents, 1)
	assert.Equal(t, "root", cfg.Agents[0].Name)
	assert.Equal(t, "auto", cfg.Agents[0].Model)
	assert.Contains(t, cfg.Agents[0].WelcomeMessage, "Hello!")
	require.Len(t, cfg.Agents[0].Toolsets, 2)
	assert.Equal(t, "shell", cfg.Agents[0].Toolsets[0].Type)
	assert.Equal(t, "filesystem", cfg.Agents[0].Toolsets[1].Type)
}

func TestBuildCreatorConfigYAML(t *testing.T) {
	t.Parallel()

	instructions := "Test instructions for the agent builder"

	data, err := buildCreatorConfigYAML(instructions)
	require.NoError(t, err)

	// Verify it can be loaded by the config loader
	cfg, err := config.Load(t.Context(), config.NewBytesSource("test", data))
	require.NoError(t, err)

	// Verify the config structure
	require.Len(t, cfg.Agents, 1)
	assert.Equal(t, "root", cfg.Agents[0].Name)
	assert.Equal(t, "auto", cfg.Agents[0].Model)
	assert.Equal(t, creatorWelcomeMessage, cfg.Agents[0].WelcomeMessage)
	assert.Equal(t, instructions, cfg.Agents[0].Instruction)
	require.Len(t, cfg.Agents[0].Toolsets, 2)
	assert.Equal(t, "shell", cfg.Agents[0].Toolsets[0].Type)
	assert.Equal(t, "filesystem", cfg.Agents[0].Toolsets[1].Type)
}

func TestBuildInstructions(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	runConfig := &config.RuntimeConfig{
		Config: config.Config{
			WorkingDir: t.TempDir(),
		},
	}

	instructions := buildInstructions(ctx, runConfig)

	// Verify the instructions contain the base instructions
	assert.Contains(t, instructions, agentBuilderInstructions)

	// Verify the instructions contain provider guidance
	assert.Contains(t, instructions, "Preferred model providers to use:")
	assert.Contains(t, instructions, "models:")
}

func TestAgent(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	// Create a runtime config with a mock env provider that has a dummy API key
	// so the auto model can resolve to a provider without needing real credentials
	runConfig := &config.RuntimeConfig{
		Config: config.Config{
			WorkingDir: t.TempDir(),
		},
		EnvProviderForTests: environment.NewEnvListProvider([]string{
			"OPENAI_API_KEY=dummy-key-for-testing",
		}),
	}

	// The auto model will be resolved based on available providers
	team, err := Agent(ctx, runConfig, "")
	require.NoError(t, err)
	require.NotNil(t, team)

	// Verify the team has a root agent
	rootAgent, err := team.DefaultAgent()
	require.NoError(t, err)
	require.NotNil(t, rootAgent)
	assert.Equal(t, "root", rootAgent.Name())

	// Verify the welcome message
	assert.Contains(t, rootAgent.WelcomeMessage(), "Hello! I'm here to create agents for you.")

	// Verify tools are available
	tools, err := rootAgent.Tools(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, tools)

	// Check that both shell and filesystem tools are available
	toolNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		toolNames = append(toolNames, tool.Name)
	}
	assert.Contains(t, toolNames, "shell")
	// Filesystem tool provides multiple tools
	assert.Contains(t, toolNames, "read_file")
	assert.Contains(t, toolNames, "write_file")
}

func TestFileWriteTracker(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	runConfig := &config.RuntimeConfig{
		Config: config.Config{
			WorkingDir: t.TempDir(),
		},
	}

	registry := createToolsetRegistry(runConfig.WorkingDir)
	require.NotNil(t, registry)

	// Create the toolset through the registry
	toolset, err := registry.CreateTool(ctx, latest.Toolset{Type: "filesystem"}, runConfig.WorkingDir, runConfig, "test-agent")
	require.NoError(t, err)
	require.NotNil(t, toolset)

	// Verify the toolset is a file write tracker
	tracker, ok := toolset.(*fileWriteTracker)
	require.True(t, ok, "expected fileWriteTracker, got %T", toolset)

	// Initially, no path should be tracked
	assert.Empty(t, tracker.LastWrittenPath())

	// Get the tools and verify write_file is present
	tools, err := tracker.Tools(ctx)
	require.NoError(t, err)

	var hasWriteFile bool
	for _, tool := range tools {
		if tool.Name == "write_file" {
			hasWriteFile = true
			break
		}
	}
	assert.True(t, hasWriteFile, "write_file tool should be present")
}

func TestBuildCreatorConfigYAML_MultilineStrings(t *testing.T) {
	t.Parallel()

	// Test with instructions containing newlines to ensure proper YAML formatting
	instructions := "Line 1\n\nLine 2\n\nLine 3"

	data, err := buildCreatorConfigYAML(instructions)
	require.NoError(t, err)

	// The YAML should properly indent multi-line strings
	yamlStr := string(data)
	t.Logf("YAML output:\n%s", yamlStr)

	// Verify the YAML can be parsed
	cfg, err := config.Load(t.Context(), config.NewBytesSource("test", data))
	require.NoError(t, err)

	// Verify the instruction is preserved correctly
	assert.Equal(t, instructions, cfg.Agents[0].Instruction)

	// Also verify welcome message with newlines is preserved
	assert.Contains(t, cfg.Agents[0].WelcomeMessage, "\n",
		"welcome message should contain newlines")
}
