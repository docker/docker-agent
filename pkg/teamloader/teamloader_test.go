package teamloader

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/model/provider/dmr"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	providerdefaults "github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/tools"
)

// skipExamples contains example files that require cloud-specific configurations
// (e.g., AWS profiles, GCP credentials) that can't be mocked with dummy env vars.
var skipExamples = map[string]string{
	"pr-reviewer-bedrock.yaml":      "requires AWS profile configuration",
	"sub-agents-from-registry.yaml": "pulls a sub-agent from a remote OCI registry",
}

func withTestProviderRegistry(opts ...Opt) []Opt {
	return append([]Opt{
		WithProviderRegistry(providerdefaults.NewDefaultRegistry()),
		WithToolsetRegistry(testToolsetRegistry()),
	}, opts...)
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

	got, warnings := getToolsForAgent(t.Context(), a, ".", &runConfig, &toolsetRegistry{}, "test-config", expander)

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

			// Use a file source so the config's directory is known: examples
			// such as instruction_file.yaml reference sibling files that are
			// resolved relative to it at load time. A temp WorkingDir still
			// redirects toolsets that write to disk (memory, RAG, cache, ...)
			// away from the examples/ tree.
			agentSource := config.NewFileSource(agentFilename)
			runConfig := &config.RuntimeConfig{}
			runConfig.WorkingDir = t.TempDir()

			teams, err := Load(catalogContext(t), agentSource, runConfig, withTestProviderRegistry()...)
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
// so t.Setenv can be called before any subtest starts. It uses a static
// gateway loader so MCP `ref:` toolsets in the examples don't trigger a live
// catalog fetch.
func gatherExampleEnvVars(t *testing.T, examples []string) map[string]bool {
	t.Helper()

	ctx := catalogContext(t)
	envs := make(map[string]bool)
	for _, agentFilename := range examples {
		agentSource, err := config.Resolve(agentFilename, nil)
		require.NoError(t, err)

		cfg, err := config.Load(ctx, agentSource)
		require.NoError(t, err)

		for _, env := range config.GatherEnvVarsForModels(ctx, cfg, environment.NewOsEnvProvider()) {
			envs[env] = true
		}
		toolEnvs, _ := config.GatherEnvVarsForTools(ctx, cfg)
		for _, env := range toolEnvs {
			envs[env] = true
		}
	}
	return envs
}

func TestLoadDefaultAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	agentSource, err := config.Resolve("default", nil)
	require.NoError(t, err)

	runConfig := &config.RuntimeConfig{
		EnvProviderForTests: environment.NewEnvListProvider([]string{
			"OPENAI_API_KEY=dummy",
		}),
	}

	teams, err := Load(t.Context(), agentSource, runConfig, withTestProviderRegistry()...)
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

			team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{}, withTestProviderRegistry(WithModelOverrides(test.overrides))...)
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

func TestTitleModelResolution(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	t.Setenv("ANTHROPIC_API_KEY", "dummy")

	t.Run("named title model", func(t *testing.T) {
		data := []byte(`models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-5
    title_model: fast
  fast:
    provider: openai
    model: gpt-4o-mini
agents:
  root:
    model: primary
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("title.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.TitleModel())
		assert.Equal(t, "openai/gpt-4o-mini", root.TitleModel().ID().String())

		// The dedicated title model comes first, the agent's own model follows
		// as a fallback so title generation still works if it is unavailable.
		models := root.TitleModels(t.Context())
		require.Len(t, models, 2)
		assert.Equal(t, "openai/gpt-4o-mini", models[0].ID().String())
		assert.Equal(t, "anthropic/claude-sonnet-4-5", models[1].ID().String())
	})

	t.Run("inline title model", func(t *testing.T) {
		data := []byte(`models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-5
    title_model: openai/gpt-4o-mini
agents:
  root:
    model: primary
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("title.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.TitleModel())
		assert.Equal(t, "openai/gpt-4o-mini", root.TitleModel().ID().String())
	})

	t.Run("no title model", func(t *testing.T) {
		data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("title.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		assert.Nil(t, root.TitleModel())

		// Without a dedicated title model, generation falls back to the
		// agent's own model.
		models := root.TitleModels(t.Context())
		require.Len(t, models, 1)
		assert.Equal(t, "openai/gpt-4o", models[0].ID().String())
	})
}

func TestCompactionModelResolution(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	t.Setenv("ANTHROPIC_API_KEY", "dummy")

	t.Run("named compaction model", func(t *testing.T) {
		data := []byte(`models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-5
    compaction_model: fast
  fast:
    provider: openai
    model: gpt-4o-mini
agents:
  root:
    model: primary
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.CompactionModel())
		assert.Equal(t, "openai/gpt-4o-mini", root.CompactionModel().ID().String())
		// The dedicated compaction model is independent of the primary model
		// that runs the conversation.
		assert.Equal(t, "anthropic/claude-sonnet-4-5", root.Model(t.Context()).ID().String())
	})

	t.Run("inline compaction model", func(t *testing.T) {
		data := []byte(`models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-5
    compaction_model: openai/gpt-4o-mini
agents:
  root:
    model: primary
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.CompactionModel())
		assert.Equal(t, "openai/gpt-4o-mini", root.CompactionModel().ID().String())
	})

	t.Run("agent-level compaction model", func(t *testing.T) {
		data := []byte(`models:
  fast:
    provider: openai
    model: gpt-4o-mini
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    compaction_model: fast
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.CompactionModel())
		assert.Equal(t, "openai/gpt-4o-mini", root.CompactionModel().ID().String())
	})

	t.Run("agent-level inline compaction model", func(t *testing.T) {
		data := []byte(`agents:
  root:
    model: anthropic/claude-sonnet-4-5
    compaction_model: openai/gpt-4o-mini
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.CompactionModel())
		assert.Equal(t, "openai/gpt-4o-mini", root.CompactionModel().ID().String())
	})

	t.Run("unknown agent-level compaction model fails the load", func(t *testing.T) {
		data := []byte(`agents:
  root:
    model: anthropic/claude-sonnet-4-5
    compaction_model: typo
    instruction: test
`)

		_, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.ErrorContains(t, err, "compaction model 'typo' not found")
	})

	t.Run("agent-level compaction model overrides model-level", func(t *testing.T) {
		data := []byte(`models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-5
    compaction_model: openai/gpt-4o-mini
agents:
  root:
    model: primary
    compaction_model: openai/gpt-4o
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.CompactionModel())
		assert.Equal(t, "openai/gpt-4o", root.CompactionModel().ID().String())
	})

	t.Run("provider-level compaction model is the default", func(t *testing.T) {
		data := []byte(`providers:
  myanthropic:
    provider: anthropic
    compaction_model: openai/gpt-4o-mini
models:
  primary:
    provider: myanthropic
    model: claude-sonnet-4-5
agents:
  root:
    model: primary
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.CompactionModel())
		assert.Equal(t, "openai/gpt-4o-mini", root.CompactionModel().ID().String())
	})

	t.Run("provider-level compaction model referencing a named model", func(t *testing.T) {
		data := []byte(`providers:
  myanthropic:
    provider: anthropic
    compaction_model: fast
models:
  primary:
    provider: myanthropic
    model: claude-sonnet-4-5
  fast:
    provider: openai
    model: gpt-4o-mini
agents:
  root:
    model: primary
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.CompactionModel())
		assert.Equal(t, "openai/gpt-4o-mini", root.CompactionModel().ID().String())
	})

	t.Run("model-level compaction model overrides provider-level", func(t *testing.T) {
		data := []byte(`providers:
  myanthropic:
    provider: anthropic
    compaction_model: openai/gpt-4o
models:
  primary:
    provider: myanthropic
    model: claude-sonnet-4-5
    compaction_model: openai/gpt-4o-mini
agents:
  root:
    model: primary
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		require.NotNil(t, root.CompactionModel())
		assert.Equal(t, "openai/gpt-4o-mini", root.CompactionModel().ID().String())
	})

	t.Run("no compaction model", func(t *testing.T) {
		data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		// Without a dedicated compaction model, compaction reuses the agent's
		// own model.
		assert.Nil(t, root.CompactionModel())
	})
}

func TestCompactionThresholdResolution(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	t.Setenv("ANTHROPIC_API_KEY", "dummy")

	t.Run("agent-level threshold", func(t *testing.T) {
		data := []byte(`agents:
  root:
    model: openai/gpt-4o
    compaction_threshold: 0.75
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("threshold.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		assert.InEpsilon(t, 0.75, root.CompactionThreshold(), 1e-9)
	})

	t.Run("model-level threshold", func(t *testing.T) {
		data := []byte(`models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-5
    compaction_threshold: 0.6
agents:
  root:
    model: primary
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("threshold.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		assert.InEpsilon(t, 0.6, root.CompactionThreshold(), 1e-9)
	})

	t.Run("model-level threshold overrides agent-level", func(t *testing.T) {
		data := []byte(`models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-5
    compaction_threshold: 0.6
agents:
  root:
    model: primary
    compaction_threshold: 0.75
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("threshold.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		assert.InEpsilon(t, 0.6, root.CompactionThreshold(), 1e-9)
	})

	t.Run("unset threshold reports zero so the default applies", func(t *testing.T) {
		data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("threshold.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		assert.Zero(t, root.CompactionThreshold())
	})
}

func TestSessionCompactionKnob(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	t.Run("enabled by default", func(t *testing.T) {
		data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("session_compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		assert.True(t, root.SessionCompaction())
	})

	t.Run("explicitly disabled", func(t *testing.T) {
		data := []byte(`agents:
  root:
    model: openai/gpt-4o
    session_compaction: false
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("session_compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		assert.False(t, root.SessionCompaction())
	})

	t.Run("explicitly enabled", func(t *testing.T) {
		data := []byte(`agents:
  root:
    model: openai/gpt-4o
    session_compaction: true
    instruction: test
`)

		team, err := Load(t.Context(), config.NewBytesSource("session_compaction.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := team.Agent("root")
		require.NoError(t, err)

		assert.True(t, root.SessionCompaction())
	})
}

func TestLoadHarnessAgentWithoutModel(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    sub_agents: [coder]
  coder:
    description: External coder
    instruction: You are a coding agent.
    harness:
      type: codex
`)

	team, err := Load(t.Context(), config.NewBytesSource("harness.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	coder, err := team.Agent("coder")
	require.NoError(t, err)
	require.True(t, coder.HasHarness())
	require.Equal(t, "codex", coder.Harness().Type)
	require.Nil(t, coder.Model(t.Context()))
}

func TestToolsetInstructions(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	agentSource, err := config.Resolve("testdata/tool-instruction.yaml", nil)
	require.NoError(t, err)

	team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{}, withTestProviderRegistry()...)
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

	team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{}, withTestProviderRegistry()...)
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

	_, err = Load(t.Context(), agentSource, runConfig, withTestProviderRegistry()...)
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

			team, err := Load(t.Context(), agentSource, &config.RuntimeConfig{}, withTestProviderRegistry(opts...)...)
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
		withTestProviderRegistry(WithPromptFiles([]string{"cli-file.md"}))...)
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
		withTestProviderRegistry(WithPromptFiles([]string{"AGENTS.md", "extra.md"}))...)
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

	got, warnings := getToolsForAgent(t.Context(), a, ".", &runConfig, testToolsetRegistry(), "test-config", expander)
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

	got, warnings := getToolsForAgent(t.Context(), a, ".", &runConfig, testToolsetRegistry(), "test-config", expander)
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

	_, err := LoadWithConfig(t.Context(), source, runConfig, withTestProviderRegistry()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes parent directory")
}

// WithWorkingDir must not leak the merged value onto a shared *RuntimeConfig
// that is reused across sessions: after Load returns, runConfig.WorkingDir
// must be exactly what the caller passed in.
func TestLoadWithConfig_WithWorkingDirDoesNotLeak(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: shell
        tools: [shell]
`)

	// Caller pins its own WorkingDir on the shared runConfig.
	callerDir := t.TempDir()
	runConfig := &config.RuntimeConfig{}
	runConfig.WorkingDir = callerDir

	// First load with a per-session override.
	sessionDir := t.TempDir()
	_, err := Load(
		t.Context(),
		config.NewBytesSource("t.yaml", data),
		runConfig,
		append(withTestProviderRegistry(), WithWorkingDir(sessionDir))...,
	)
	require.NoError(t, err)
	assert.Equal(t, callerDir, runConfig.WorkingDir,
		"per-session WithWorkingDir must be restored on return")

	// Second load with no override must see the caller's original WorkingDir,
	// not the previous session's value.
	_, err = Load(
		t.Context(),
		config.NewBytesSource("t.yaml", data),
		runConfig,
		withTestProviderRegistry()...,
	)
	require.NoError(t, err)
	assert.Equal(t, callerDir, runConfig.WorkingDir)
}

// TestLoadRetainsAgentConfig verifies the loader retains the raw resolved
// per-agent config on the team (team.WithAgentConfigs) so the agent inspector
// can surface declared toolset allow-lists, limits and flags. It uses a
// built-in toolset (shell) and an openai model so no network access is needed.
func TestLoadRetainsAgentConfig(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    max_iterations: 7
    toolsets:
      - type: shell
        tools: [shell]
`)

	team, err := Load(t.Context(), config.NewBytesSource("inspector.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	cfg, ok := team.AgentConfig("root")
	require.True(t, ok, "loader must retain the resolved agent config")
	assert.Equal(t, 7, cfg.MaxIterations)
	require.Len(t, cfg.Toolsets, 1)
	assert.Equal(t, "shell", cfg.Toolsets[0].Type)
	assert.Equal(t, []string{"shell"}, cfg.Toolsets[0].Tools)

	_, ok = team.AgentConfig("missing")
	assert.False(t, ok, "unknown agent reports no retained config")
}

// TestLoadPropagatesMaxToolResultTokens verifies the opt-in per-tool-result
// cap travels from the YAML config to the built agent, where session
// constructors read it via agent.MaxToolResultTokens().
func TestLoadPropagatesMaxToolResultTokens(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    max_tool_result_tokens: 512
`)

	team, err := Load(t.Context(), config.NewBytesSource("cap.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	agt, err := team.Agent("root")
	require.NoError(t, err)
	assert.Equal(t, 512, agt.MaxToolResultTokens())
}

// TestLoadPropagatesStructuredOutput verifies the original structured-output
// config (including tool mode) travels from the YAML to the built agent,
// where the runtime reads it via agent.StructuredOutput().
func TestLoadPropagatesStructuredOutput(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    structured_output:
      mode: tool
      name: result
      schema:
        type: object
        properties:
          answer:
            type: string
        required: ["answer"]
`)

	team, err := Load(t.Context(), config.NewBytesSource("so.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	agt, err := team.Agent("root")
	require.NoError(t, err)
	so := agt.StructuredOutput()
	require.NotNil(t, so, "agent must retain the original structured-output config")
	assert.Equal(t, "result", so.Name)
	assert.True(t, so.ToolMode())
	assert.Contains(t, so.Schema, "properties")
}

// TestLoadRejectsUncompilableToolModeSchema pins the load-time failure for
// tool-mode structured output with a schema gojsonschema cannot compile.
func TestLoadRejectsUncompilableToolModeSchema(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    structured_output:
      mode: tool
      name: result
      schema:
        type: object
        properties:
          answer:
            type: 42
`)

	_, err := Load(t.Context(), config.NewBytesSource("bad-schema.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.ErrorContains(t, err, "agent root: structured_output")
}

// TestLoadPropagatesSafetyDefaults verifies the author-declared safety
// defaults travel from the YAML config to the built team: runtime.safety
// lands on the team (team.RuntimeSafety) and agents.<name>.safety on the
// agent (agent.Safety), where session constructors resolve them.
func TestLoadPropagatesSafetyDefaults(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`runtime:
  safety: autonomous
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    safety: balanced
  headless:
    model: openai/gpt-4o
    instruction: test
    safety: restricted
  careful:
    model: openai/gpt-4o
    instruction: test
`)

	team, err := Load(t.Context(), config.NewBytesSource("safety.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	assert.Equal(t, latest.SafetyModeAutonomous, team.RuntimeSafety())

	root, err := team.Agent("root")
	require.NoError(t, err)
	assert.Equal(t, latest.SafetyModeBalanced, root.Safety())

	headless, err := team.Agent("headless")
	require.NoError(t, err)
	assert.Equal(t, latest.SafetyModeRestricted, headless.Safety())

	careful, err := team.Agent("careful")
	require.NoError(t, err)
	assert.Empty(t, careful.Safety(), "agent without a safety default carries none of its own")
}

// TestLoadRejectsInvalidSafety pins the load-time failure: a non-canonical
// safety value anywhere in the config must fail loading with an error that
// names the offending field.
func TestLoadRejectsInvalidSafety(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	tests := []struct {
		name    string
		data    string
		wantErr string
	}{
		{
			name: "runtime scope",
			data: `runtime:
  safety: yolo
agents:
  root:
    model: openai/gpt-4o
    instruction: test
`,
			wantErr: "runtime.safety: invalid safety mode \"yolo\"",
		},
		{
			name: "agent scope",
			data: `agents:
  root:
    model: openai/gpt-4o
    instruction: test
    safety: safe-auto
`,
			wantErr: "agents.root.safety: invalid safety mode \"safe-auto\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(t.Context(), config.NewBytesSource("bad.yaml", []byte(tt.data)), &config.RuntimeConfig{}, withTestProviderRegistry()...)
			require.ErrorContains(t, err, tt.wantErr)
			require.ErrorContains(t, err, "strict, balanced, restricted, autonomous")
		})
	}
}

// TestLoadWithConfig_GlobalProviders covers user-level custom providers
// (seeded into the runtime config from the user config file): they must
// resolve inline `provider/model` references in any agent config, while
// agent-file definitions with the same name keep precedence.
func TestLoadWithConfig_GlobalProviders(t *testing.T) {
	t.Parallel()

	globalProviders := map[string]latest.ProviderConfig{
		"myprovider": {
			BaseURL:  "https://llm.corp.example.com/v1",
			APIType:  "openai_chatcompletions",
			TokenKey: "MYPROVIDER_API_KEY",
		},
	}
	env := environment.NewMapEnvProvider(map[string]string{"MYPROVIDER_API_KEY": "dummy"})

	t.Run("resolves inline model reference", func(t *testing.T) {
		t.Parallel()

		data := []byte(`agents:
  root:
    model: myprovider/corp-model
    instruction: test
`)
		runConfig := &config.RuntimeConfig{
			Config:              config.Config{Providers: globalProviders},
			EnvProviderForTests: env,
		}

		result, err := LoadWithConfig(t.Context(), config.NewBytesSource("global-providers.yaml", data), runConfig, withTestProviderRegistry()...)
		require.NoError(t, err)

		root, err := result.Team.Agent("root")
		require.NoError(t, err)

		models := root.ConfiguredModels()
		require.Len(t, models, 1)
		assert.Equal(t, "myprovider/corp-model", models[0].ID().String())

		require.Contains(t, result.Providers, "myprovider")
		assert.Equal(t, "https://llm.corp.example.com/v1", result.Providers["myprovider"].BaseURL)
	})

	t.Run("resolves --model override", func(t *testing.T) {
		t.Parallel()

		// The agent file references another provider entirely; the CLI
		// override must resolve through the merged global provider (the
		// `docker agent new|run --model myprovider/mymodel` path).
		data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
		runConfig := &config.RuntimeConfig{
			Config:              config.Config{Providers: globalProviders},
			EnvProviderForTests: env,
		}

		result, err := LoadWithConfig(t.Context(), config.NewBytesSource("global-providers.yaml", data), runConfig,
			withTestProviderRegistry(WithModelOverrides([]string{"myprovider/corp-model"}))...)
		require.NoError(t, err)

		root, err := result.Team.Agent("root")
		require.NoError(t, err)

		models := root.ConfiguredModels()
		require.Len(t, models, 1)
		assert.Equal(t, "myprovider/corp-model", models[0].ID().String())
	})

	t.Run("missing token variable fails the preflight check", func(t *testing.T) {
		t.Parallel()

		data := []byte(`agents:
  root:
    model: myprovider/corp-model
    instruction: test
`)
		runConfig := &config.RuntimeConfig{
			Config:              config.Config{Providers: globalProviders},
			EnvProviderForTests: &noEnvProvider{},
		}

		_, err := LoadWithConfig(t.Context(), config.NewBytesSource("global-providers.yaml", data), runConfig, withTestProviderRegistry()...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MYPROVIDER_API_KEY")
	})

	t.Run("agent file definition wins", func(t *testing.T) {
		t.Parallel()

		data := []byte(`providers:
  myprovider:
    base_url: https://file.example.com/v1
models:
  corp:
    provider: myprovider
    model: corp-model
agents:
  root:
    model: corp
    instruction: test
`)
		runConfig := &config.RuntimeConfig{
			Config:              config.Config{Providers: globalProviders},
			EnvProviderForTests: env,
		}

		result, err := LoadWithConfig(t.Context(), config.NewBytesSource("global-providers.yaml", data), runConfig, withTestProviderRegistry()...)
		require.NoError(t, err)

		require.Contains(t, result.Providers, "myprovider")
		assert.Equal(t, "https://file.example.com/v1", result.Providers["myprovider"].BaseURL)
	})
}

func TestLoadWithConfig_GlobalHooks(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
`)
	runConfig := &config.RuntimeConfig{
		Config: config.Config{
			GlobalHooks: &latest.HooksConfig{
				SessionStart: []latest.HookDefinition{{Type: "command", Command: "global"}},
			},
		},
	}

	team, err := Load(t.Context(), config.NewBytesSource("global-hooks.yaml", data), runConfig, withTestProviderRegistry()...)
	require.NoError(t, err)

	root, err := team.Agent("root")
	require.NoError(t, err)
	require.NotNil(t, root.Hooks())
	require.Len(t, root.Hooks().SessionStart, 1)
	assert.Equal(t, "global", root.Hooks().SessionStart[0].Command)
}

func TestLoadWithConfig_MergesAgentGlobalAndCLIHooks(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    hooks:
      session_start:
        - type: command
          command: agent
      pre_tool_use:
        - matcher: shell
          hooks:
            - type: command
              command: agent-pre
`)
	runConfig := &config.RuntimeConfig{
		Config: config.Config{
			GlobalHooks: &latest.HooksConfig{
				SessionStart: []latest.HookDefinition{{Type: "command", Command: "global"}},
				PreToolUse: []latest.HookMatcherConfig{{
					Matcher: "edit_file",
					Hooks:   []latest.HookDefinition{{Type: "command", Command: "global-pre"}},
				}},
			},
			HookSessionStart: []string{"cli"},
			HookPreToolUse:   []string{"cli-pre"},
		},
	}

	team, err := Load(t.Context(), config.NewBytesSource("global-hooks.yaml", data), runConfig, withTestProviderRegistry()...)
	require.NoError(t, err)

	root, err := team.Agent("root")
	require.NoError(t, err)
	hooks := root.Hooks()
	require.NotNil(t, hooks)

	require.Len(t, hooks.SessionStart, 3)
	assert.Equal(t, "agent", hooks.SessionStart[0].Command)
	assert.Equal(t, "global", hooks.SessionStart[1].Command)
	assert.Equal(t, "cli", hooks.SessionStart[2].Command)

	require.Len(t, hooks.PreToolUse, 3)
	assert.Equal(t, "shell", hooks.PreToolUse[0].Matcher)
	assert.Equal(t, "agent-pre", hooks.PreToolUse[0].Hooks[0].Command)
	assert.Equal(t, "edit_file", hooks.PreToolUse[1].Matcher)
	assert.Equal(t, "global-pre", hooks.PreToolUse[1].Hooks[0].Command)
	assert.Empty(t, hooks.PreToolUse[2].Matcher)
	assert.Equal(t, "cli-pre", hooks.PreToolUse[2].Hooks[0].Command)
}

// countingTransport wraps a base RoundTripper and counts how many times
// RoundTrip is called. Mirrors the helper in
// pkg/model/provider/anthropic/wrap_transport_test.go (unexported there, so
// duplicated here for this package's own end-to-end assertion).
type countingTransport struct {
	base  http.RoundTripper
	calls atomic.Int64
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.base.RoundTrip(req)
}

// writeMinimalAnthropicSSE writes a bare-minimum valid Anthropic SSE stream so
// the streaming client does not error before the transport wrapper can be
// observed.
func writeMinimalAnthropicSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)

	writeEvent := func(eventType string, payload map[string]any) {
		data, _ := json.Marshal(payload)
		_, _ = w.Write([]byte("event: " + eventType + "\n"))
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeEvent("message_start", map[string]any{
		"type":    "message_start",
		"message": map[string]any{"id": "msg_test", "model": "claude-test", "role": "assistant", "type": "message", "content": []any{}, "stop_reason": nil, "usage": map[string]any{"input_tokens": 5, "output_tokens": 0}},
	})
	writeEvent("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
	writeEvent("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "text_delta", "text": "hi"},
	})
	writeEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})
	writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 1},
	})
	writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

// TestWithModelOptions_TransportWrapperReachesAgentModel is the regression
// guard for the teamloader-level model-options passthrough: it proves that an
// options.Opt supplied via WithModelOptions (e.g.
// options.WithHTTPTransportWrapper, used to inject gateway auth
// provider-agnostically) actually reaches the HTTP client of a model
// constructed through Load — not just teamloader's own built-in opts
// (WithGateway, WithStructuredOutput, WithProviders). Before this passthrough
// existed, there was no way for an embedder to wrap the transport of
// teamloader-constructed models at all; this test exercises the full Load ->
// agent -> provider -> HTTP round trip to prove the wrapper is live, not just
// that WithModelOptions compiles.
func TestWithModelOptions_TransportWrapperReachesAgentModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeMinimalAnthropicSSE(w)
	}))
	defer server.Close()

	t.Setenv("ANTHROPIC_API_KEY", "dummy")

	data := []byte(`models:
  primary:
    provider: anthropic
    model: claude-sonnet-4-5
    base_url: ` + server.URL + `
agents:
  root:
    model: primary
    instruction: test
`)

	var counter countingTransport
	opts := append(withTestProviderRegistry(), WithModelOptions(
		options.WithHTTPTransportWrapper(func(base http.RoundTripper) http.RoundTripper {
			counter.base = base
			return &counter
		}),
	))

	team, err := Load(t.Context(), config.NewBytesSource("transport-wrapper.yaml", data), &config.RuntimeConfig{}, opts...)
	require.NoError(t, err)

	root, err := team.Agent("root")
	require.NoError(t, err)

	stream, err := root.Model(t.Context()).CreateChatCompletionStream(t.Context(), []chat.Message{
		{Role: chat.MessageRoleUser, Content: "hello"},
	}, nil)
	require.NoError(t, err)
	defer stream.Close()

	for {
		if _, err := stream.Recv(); err != nil {
			break
		}
	}

	assert.Positive(t, counter.calls.Load(), "transport wrapper supplied via WithModelOptions should have been invoked when the agent's model made a request")
}

// Concurrent loads that share one *RuntimeConfig — the shape the API server
// uses, one config for every session — must each see their own working
// directory. Toolsets read it off runConfig, so a load that mutates the
// shared value hands the wrong directory to another session's shell,
// filesystem and git tools.
func TestLoadWithConfig_WithWorkingDirIsConcurrencySafe(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: recorder
`)

	var mu sync.Mutex
	seen := map[string]string{}
	recorder := NewToolsetRegistry(map[string]ToolsetCreator{
		"recorder": func(_ context.Context, _ latest.Toolset, _ string, runConfig *config.RuntimeConfig, agentName string) (tools.ToolSet, error) {
			// Hold the observed value across a scheduling point to widen
			// the window in which a competing load could clobber it.
			observed := runConfig.WorkingDir
			runtime.Gosched()
			mu.Lock()
			defer mu.Unlock()
			seen[observed] = agentName
			return &mockToolSet{}, nil
		},
	})

	callerDir := t.TempDir()
	runConfig := &config.RuntimeConfig{}
	runConfig.WorkingDir = callerDir

	const loads = 8
	sessionDirs := make([]string, loads)
	for i := range sessionDirs {
		sessionDirs[i] = t.TempDir()
	}

	var wg sync.WaitGroup
	errs := make([]error, loads)
	for i, dir := range sessionDirs {
		wg.Go(func() {
			_, errs[i] = Load(
				t.Context(),
				config.NewBytesSource("t.yaml", data),
				runConfig,
				WithProviderRegistry(providerdefaults.NewDefaultRegistry()),
				WithToolsetRegistry(recorder),
				WithWorkingDir(dir),
			)
		})
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "load %d", i)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, dir := range sessionDirs {
		assert.Contains(t, seen, dir,
			"each session's toolsets must be built with that session's working directory")
	}
	assert.Equal(t, callerDir, runConfig.WorkingDir,
		"the shared RuntimeConfig must never be mutated")
}
