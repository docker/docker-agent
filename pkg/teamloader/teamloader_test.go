package teamloader

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
	"github.com/docker/docker-agent/pkg/tools"
)

// skipExamples contains example files that require cloud-specific configurations
// (e.g., AWS profiles, GCP credentials) that can't be mocked with dummy env vars.
var skipExamples = map[string]string{
	"pr-reviewer-bedrock.yaml": "requires AWS profile configuration",
}

func collectExamples(t *testing.T) []string {
	t.Helper()

	var files []string
	err := filepath.WalkDir(filepath.Join("..", "..", "examples"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(path) == ".yaml" {
			if reason, skip := skipExamples[filepath.Base(path)]; skip {
				t.Logf("Skipping %s: %s", path, reason)
				return nil
			}
			files = append(files, path)
		}
		return nil
	})
	require.NoError(t, err)
	assert.NotEmpty(t, files)

	return files
}

type noEnvProvider struct{}

func (p *noEnvProvider) Get(context.Context, string) (string, bool) { return "", false }

func TestGetToolsForAgent_ContinuesOnCreateToolError(t *testing.T) {
	t.Parallel()

	// Agent with a bogus toolset type to force createTool error without network access
	a := &latest.AgentConfig{
		Instruction: "test",
		Toolsets:    []latest.Toolset{{Type: "does-not-exist"}},
	}

	runConfig := config.RuntimeConfig{
		EnvProviderForTests: &noEnvProvider{},
	}

	expander := js.NewJsExpander(runConfig.EnvProvider())

	got, warnings := getToolsForAgent(t.Context(), a, ".", &runConfig, NewToolsetRegistry(), "test-config", expander)

	require.Empty(t, got)
	require.NotEmpty(t, warnings)
	require.Contains(t, warnings[0], "toolset does-not-exist failed")
}

func TestLoadExamples(t *testing.T) {
	examples := collectExamples(t)

	// Set every env var referenced by the examples to a dummy value so model
	// and tool initialisation succeeds without real credentials.
	for env := range gatherExampleEnvVars(t, examples) {
		t.Setenv(env, "dummy")
	}

	for _, agentFilename := range examples {
		t.Run(agentFilename, func(t *testing.T) {
			t.Parallel()

			data, err := os.ReadFile(agentFilename)
			require.NoError(t, err)

			// Examples must not pin a version: they should always parse with
			// the latest config schema.
			var v struct {
				Version string `yaml:"version"`
			}
			require.NoError(t, yaml.Unmarshal(data, &v))
			require.Empty(t, v.Version, "example %s should not define a version", agentFilename)

			// Use a bytes source (ParentDir == "") plus a temp WorkingDir so
			// toolsets that write to disk (memory, RAG, cache, ...) land in
			// the temp dir instead of the examples/ tree.
			agentSource := config.NewBytesSource(agentFilename, data)
			runConfig := &config.RuntimeConfig{}
			runConfig.WorkingDir = t.TempDir()

			teams, err := Load(t.Context(), agentSource, runConfig)
			if errors.Is(err, dmr.ErrNotInstalled) {
				t.Skipf("Skipping %s: Docker Model Runner not installed", agentFilename)
			}
			require.NoError(t, err)
			assert.NotEmpty(t, teams)
		})
	}
}

// gatherExampleEnvVars returns the union of env vars referenced by the given
// example files (both for models and toolsets). The set is collected up-front
// so t.Setenv can be called before any subtest starts.
func gatherExampleEnvVars(t *testing.T, examples []string) map[string]bool {
	t.Helper()

	envs := make(map[string]bool)
	for _, agentFilename := range examples {
		agentSource, err := config.Resolve(agentFilename, nil)
		require.NoError(t, err)

		cfg, err := config.Load(t.Context(), agentSource)
		require.NoError(t, err)

		for _, env := range config.GatherEnvVarsForModels(cfg) {
			envs[env] = true
		}
		toolEnvs, _ := config.GatherEnvVarsForTools(t.Context(), cfg)
		for _, env := range toolEnvs {
			envs[env] = true
		}
	}
	return envs
}

func TestLoadDefaultAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	agentSource, err := config.Resolve("default", nil)
	require.NoError(t, err)

	runConfig := &config.RuntimeConfig{
		EnvProviderForTests: environment.NewEnvListProvider([]string{
			"OPENAI_API_KEY=dummy",
		}),
	}

	teams, err := Load(t.Context(), agentSource, runConfig)
	require.NoError(t, err)
	require.NotEmpty(t, teams)
}

func TestOverrideModel(t *testing.T) {
	tests := []struct {
		overrides   []string
		expected    string
		expectedErr string
	}{
		{
			overrides: []string{"anthropic/claude-4-6"},
			expected:  "anthropic/claude-4-6",
		},
		{
			overrides: []string{"root=anthropic/claude-4-6"},
			expected:  "anthropic/claude-4-6",
		},
		{
			overrides:   []string{"missing=anthropic/claude-4-6"},
			expectedErr: "unknown agent 'missing'",
		},
	}

	t.Setenv("OPENAI_API_KEY", "asdf")
	t.Setenv("ANTHROPIC_API_KEY", "asdf")

	for _, test := range tests {
		t.Run(test.expected, func(t *testing.T) {
			t.Parallel()

			agentSource, err := config.Resolve("testdata/basic.yaml", nil)
			require.NoError(t, err)

			team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{}, WithModelOverrides(test.overrides))
			if test.expectedErr != "" {
				require.Contains(t, err.Error(), test.expectedErr)
			} else {
				require.NoError(t, err)
				rootAgent, err := team.Agent("root")
				require.NoError(t, err)
				require.Equal(t, test.expected, rootAgent.Model(t.Context()).ID().String())
			}
		})
	}
}

func TestToolsetInstructions(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	agentSource, err := config.Resolve("testdata/tool-instruction.yaml", nil)
	require.NoError(t, err)

	team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{})
	require.NoError(t, err)

	agent, err := team.Agent("root")
	require.NoError(t, err)

	toolsets := agent.ToolSets()
	require.Len(t, toolsets, 1)

	instructions := tools.GetInstructions(toolsets[0])
	expected := "Dummy fetch tool instruction"
	require.Equal(t, expected, instructions)
}

// TestInstructionExpansion verifies that ${env.X} placeholders are expanded
// at load time both in agent.instruction and in toolsets[*].instruction.
// See https://github.com/docker/docker-agent/issues/2614.
func TestInstructionExpansion(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	t.Setenv("USER", "alice")

	agentSource, err := config.Resolve("testdata/instruction-expansion.yaml", nil)
	require.NoError(t, err)

	team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{})
	require.NoError(t, err)

	rootAgent, err := team.Agent("root")
	require.NoError(t, err)

	// agents.<name>.instruction must be expanded.
	assert.Equal(t, "Hello alice, you are running in staging", rootAgent.Instruction())

	// toolsets[*].instruction must also be expanded.
	toolsets := rootAgent.ToolSets()
	require.Len(t, toolsets, 1)
	assert.Equal(t, "Fetch as alice", tools.GetInstructions(toolsets[0]))
}

func TestAutoModelFallbackError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Skipping docker CLI shim test on Windows")
	}

	tempDir := t.TempDir()
	dockerPath := filepath.Join(tempDir, "docker")
	script := "#!/bin/sh\n" +
		"printf 'unknown flag: --json\\n\\nUsage:  docker [OPTIONS] COMMAND [ARG...]\\n\\nRun '\\''docker --help'\\'' for more information\\n' >&2\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(dockerPath, []byte(script), 0o755))

	t.Setenv("PATH", tempDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MODEL_RUNNER_HOST", "")

	agentSource, err := config.Resolve("testdata/auto-model.yaml", nil)
	require.NoError(t, err)

	// Use noEnvProvider to ensure no API keys are available,
	// so DMR is the only fallback option.
	runConfig := &config.RuntimeConfig{
		EnvProviderForTests: &noEnvProvider{},
	}

	_, err = Load(t.Context(), agentSource, runConfig)
	require.Error(t, err)

	var autoErr *config.AutoModelFallbackError
	require.ErrorAs(t, err, &autoErr, "expected AutoModelFallbackError when auto model selection fails")
}

func TestIsThinkingBudgetDisabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		budget   *latest.ThinkingBudget
		expected bool
	}{
		{"nil budget", nil, false},
		{"Tokens=0 (disabled)", &latest.ThinkingBudget{Tokens: 0}, true},
		{"Effort=none (disabled)", &latest.ThinkingBudget{Effort: "none"}, true},
		{"Tokens=8192 (enabled)", &latest.ThinkingBudget{Tokens: 8192}, false},
		{"Effort=medium (enabled)", &latest.ThinkingBudget{Effort: "medium"}, false},
		{"Effort=high (enabled)", &latest.ThinkingBudget{Effort: "high"}, false},
		{"Effort=low (enabled)", &latest.ThinkingBudget{Effort: "low"}, false},
		{"Tokens=-1 (dynamic)", &latest.ThinkingBudget{Tokens: -1}, false},
		{"Tokens=0 with Effort=medium", &latest.ThinkingBudget{Tokens: 0, Effort: "medium"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.budget.IsDisabled()
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestWithPromptFiles(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	tests := []struct {
		name           string
		cliPromptFiles []string
		expected       []string
	}{
		{
			name:           "no CLI prompt files",
			cliPromptFiles: nil,
			expected:       []string{}, // basic.yaml has no add_prompt_files
		},
		{
			name:           "single CLI prompt file",
			cliPromptFiles: []string{"AGENTS.md"},
			expected:       []string{"AGENTS.md"},
		},
		{
			name:           "multiple CLI prompt files",
			cliPromptFiles: []string{"AGENTS.md", "CLAUDE.md"},
			expected:       []string{"AGENTS.md", "CLAUDE.md"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agentSource, err := config.Resolve("testdata/basic.yaml", nil)
			require.NoError(t, err)

			var opts []Opt
			if len(tt.cliPromptFiles) > 0 {
				opts = append(opts, WithPromptFiles(tt.cliPromptFiles))
			}

			team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{}, opts...)
			require.NoError(t, err)

			rootAgent, err := team.Agent("root")
			require.NoError(t, err)

			assert.Equal(t, tt.expected, rootAgent.AddPromptFiles())
		})
	}
}

func TestWithPromptFilesMergesWithConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	// Create a temp agent file with add_prompt_files configured
	tempDir := t.TempDir()
	agentFile := filepath.Join(tempDir, "agent.yaml")
	agentYAML := `version: "2"
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    add_prompt_files:
      - config-file.md
`
	require.NoError(t, os.WriteFile(agentFile, []byte(agentYAML), 0o644))

	agentSource, err := config.Resolve(agentFile, nil)
	require.NoError(t, err)

	// Load with CLI prompt files - should merge with config
	team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{},
		WithPromptFiles([]string{"cli-file.md"}))
	require.NoError(t, err)

	rootAgent, err := team.Agent("root")
	require.NoError(t, err)

	// Config files come first, then CLI files
	expected := []string{"config-file.md", "cli-file.md"}
	assert.Equal(t, expected, rootAgent.AddPromptFiles())
}

func TestWithPromptFilesDeduplicates(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	// Create a temp agent file with add_prompt_files configured
	tempDir := t.TempDir()
	agentFile := filepath.Join(tempDir, "agent.yaml")
	agentYAML := `version: "2"
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    add_prompt_files:
      - AGENTS.md
      - CLAUDE.md
`
	require.NoError(t, os.WriteFile(agentFile, []byte(agentYAML), 0o644))

	agentSource, err := config.Resolve(agentFile, nil)
	require.NoError(t, err)

	// CLI specifies a file that's already in config - should deduplicate
	team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{},
		WithPromptFiles([]string{"AGENTS.md", "extra.md"}))
	require.NoError(t, err)

	rootAgent, err := team.Agent("root")
	require.NoError(t, err)

	// AGENTS.md should only appear once (from config), extra.md added at end
	expected := []string{"AGENTS.md", "CLAUDE.md", "extra.md"}
	assert.Equal(t, expected, rootAgent.AddPromptFiles())
}

func TestGetToolsForAgent_MultipleLSPToolsetsAreCombined(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Instruction: "test",
		Toolsets: []latest.Toolset{
			{
				Type:      "lsp",
				Command:   "gopls",
				Version:   "golang/tools@v0.21.0",
				FileTypes: []string{".go"},
			},
			{
				Type:      "lsp",
				Command:   "gopls",
				Version:   "golang/tools@v0.21.0",
				FileTypes: []string{".mod"},
			},
		},
	}

	runConfig := config.RuntimeConfig{
		EnvProviderForTests: &noEnvProvider{},
	}

	expander := js.NewJsExpander(runConfig.EnvProvider())

	got, warnings := getToolsForAgent(t.Context(), a, ".", &runConfig, NewDefaultToolsetRegistry(), "test-config", expander)
	require.Empty(t, warnings)

	// Should have exactly one toolset (the multiplexer)
	require.Len(t, got, 1)

	// Verify that we get no duplicate tool names
	allTools, err := got[0].Tools(t.Context())
	require.NoError(t, err)

	seen := make(map[string]bool)
	for _, tool := range allTools {
		assert.False(t, seen[tool.Name], "duplicate tool name: %s", tool.Name)
		seen[tool.Name] = true
	}

	// Verify LSP tools are present
	assert.True(t, seen["lsp_hover"])
	assert.True(t, seen["lsp_definition"])
}

func TestGetToolsForAgent_SingleLSPToolsetNotWrapped(t *testing.T) {
	t.Parallel()

	a := &latest.AgentConfig{
		Instruction: "test",
		Toolsets: []latest.Toolset{
			{
				Type:      "lsp",
				Command:   "gopls",
				Version:   "golang/tools@v0.21.0",
				FileTypes: []string{".go"},
			},
		},
	}

	runConfig := config.RuntimeConfig{
		EnvProviderForTests: &noEnvProvider{},
	}

	expander := js.NewJsExpander(runConfig.EnvProvider())

	got, warnings := getToolsForAgent(t.Context(), a, ".", &runConfig, NewDefaultToolsetRegistry(), "test-config", expander)
	require.Empty(t, warnings)

	// Should have exactly one toolset that provides LSP tools.
	require.Len(t, got, 1)

	allTools, err := got[0].Tools(t.Context())
	require.NoError(t, err)

	var names []string
	for _, tool := range allTools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, "lsp_hover")
	assert.Contains(t, names, "lsp_definition")
}

func TestExternalDepthContext(t *testing.T) {
	t.Parallel()

	// Default depth is 0
	ctx := t.Context()
	assert.Equal(t, 0, externalDepthFromContext(ctx))

	// Setting depth works
	ctx = contextWithExternalDepth(ctx, 3)
	assert.Equal(t, 3, externalDepthFromContext(ctx))

	// Nested overrides
	ctx = contextWithExternalDepth(ctx, 7)
	assert.Equal(t, 7, externalDepthFromContext(ctx))
}

func TestLoadWithConfig_CachePathTraversal(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	tmpDir := t.TempDir()

	// Create a config file with a path traversal attempt
	configPath := filepath.Join(tmpDir, "agent.yaml")
	configContent := `
agents:
  root:
    model: openai/gpt-4o
    description: Test agent
    instruction: You are a test agent.
    cache:
      enabled: true
      path: ../../../../etc/passwd
`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	source := config.NewFileSource(configPath)
	runConfig := &config.RuntimeConfig{}
	runConfig.WorkingDir = tmpDir

	_, err := LoadWithConfig(t.Context(), source, runConfig)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes parent directory")
}
