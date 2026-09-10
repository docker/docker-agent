package config

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const requirementsYAML = `
providers:
  corp:
    provider: anthropic
    base_url: https://llm.corp.example
models:
  main:
    provider: corp
    model: claude-sonnet-4-5
    title_model: openai/gpt-4o-mini
  cheap:
    provider: mistral
    model: mistral-small
  router:
    provider: anthropic
    model: claude-haiku-4-5
    routing:
      - model: google/gemini-2.5-flash
        examples: [write code]
toolsets:
  fs:
    type: filesystem
agents:
  root:
    model: main
    instruction: hi
    toolsets:
      - type: shell
        toon: ".*"
        model: google/gemini-2.5-pro
      - type: mcp
        command: foo
        defer: true
    use_toolsets: [fs]
    fallback:
      models: [cheap, amazon-bedrock/nova]
    compaction_model: dmr/ai/qwen3
    sub_agents: [helper, reviewer:myorg/reviewer:v1]
    hooks:
      pre_tool_use:
        - matcher: shell
          hooks:
            - type: command
              command: echo
    skills:
      - name: deploy
        description: deploy things
        context: fork
        model: amazon-bedrock/nova-lite
        instructions: go
  helper:
    model: cheap
    instruction: hi
    harness:
      type: codex
    code_mode_tools: true
`

func TestRequires(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", []byte(requirementsYAML)))
	require.NoError(t, err)

	r := Requires(cfg)

	assert.Equal(t, map[string][]string{
		// Custom provider and alias resolve to the registry key they are served by.
		"anthropic":      {`models.main (provider "corp")`, "models.router"},
		"openai":         {`models.cheap (provider "mistral")`, "models.main.title_model"},
		"google":         {"models.google/gemini-2.5-flash", "agents.root.toolsets[0].model"}, // routing targets are normalised into models by Load
		"amazon-bedrock": {"agents.root.fallback.models[1]", "agents.root.skills.inline[0].model"},
		"dmr":            {"agents.root.compaction_model"},
	}, r.Providers)

	assert.Equal(t, map[string][]string{
		"filesystem": {"toolsets.fs", "agents.root.toolsets[2]"},
		"shell":      {"agents.root.toolsets[0]"},
		"mcp":        {"agents.root.toolsets[1]"},
	}, r.Toolsets)

	assert.Equal(t, map[Feature][]string{
		FeatureExternalAgents: {"agents.root.sub_agents[1]"},
		FeatureHooks:          {"agents.root.hooks"},
		FeatureSkills:         {"agents.root.skills"},
		FeatureHarness:        {"agents.helper.harness"},
		FeatureCodeMode:       {"agents.helper.code_mode_tools"},
		FeatureToon:           {"agents.root.toolsets[0].toon"},
		FeatureDeferredTools:  {"agents.root.toolsets[1].defer"},
	}, r.Features)
}

func TestRequirements_Check(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", []byte(requirementsYAML)))
	require.NoError(t, err)
	r := Requires(cfg)

	has := func(allowed ...string) func(string) bool {
		return func(s string) bool { return slices.Contains(allowed, s) }
	}

	t.Run("all satisfied", func(t *testing.T) {
		t.Parallel()
		err := r.Check(
			has("anthropic", "openai", "google", "amazon-bedrock", "dmr"),
			has("filesystem", "shell", "mcp"),
			[]Feature{FeatureExternalAgents, FeatureHooks, FeatureSkills, FeatureHarness, FeatureCodeMode, FeatureToon, FeatureDeferredTools},
		)
		require.NoError(t, err)
	})

	t.Run("reports everything missing at once", func(t *testing.T) {
		t.Parallel()
		err := r.Check(has("anthropic"), has("shell", "filesystem"), []Feature{FeatureSkills})
		require.Error(t, err)

		var u *UnsupportedError
		require.ErrorAs(t, err, &u)
		assert.Len(t, u.Providers, 4)
		assert.Len(t, u.Toolsets, 1)
		assert.Len(t, u.Features, 6)

		msg := err.Error()
		assert.Contains(t, msg, `provider "openai" at models.cheap (provider "mistral"), models.main.title_model`)
		assert.Contains(t, msg, `toolset type "mcp" at agents.root.toolsets[1]`)
		assert.Contains(t, msg, `feature "hooks" at agents.root.hooks`)
		assert.NotContains(t, msg, `"anthropic"`)
		assert.NotContains(t, msg, `"skills"`)
	})
}

func TestRequires_FirstAvailableIsDeferred(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewBytesSource("agent.yaml", []byte(`
models:
  pick:
    first_available:
      - openai/gpt-4o
      - anthropic/claude-sonnet-4-5
agents:
  root:
    model: pick
    instruction: hi
`)))
	require.NoError(t, err)

	r := Requires(cfg)
	assert.Empty(t, r.Providers, "first_available is resolved against the environment at load time")
}
