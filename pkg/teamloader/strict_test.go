package teamloader

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/js"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/anthropic"
	"github.com/docker/docker-agent/pkg/tools/builtin/think"
	"github.com/docker/docker-agent/pkg/tools/toon"
)

const strictYAML = `
models:
  main:
    provider: anthropic
    model: claude-sonnet-4-5
agents:
  root:
    model: main
    instruction: hi
    toolsets:
      - type: think
      - type: shell
    fallback:
      models: [openai/gpt-4o]
    hooks:
      pre_tool_use:
        - matcher: shell
          hooks:
            - type: command
              command: echo
`

func strictTestOpts(extra ...Opt) []Opt {
	anthropicOnly := provider.NewRegistry(map[string]provider.Factory{"anthropic": provider.Adapt(anthropic.NewClient)})
	thinkOnly := NewToolsetRegistry(map[string]ToolsetCreator{"think": Creator(think.CreateToolSet)})
	return append([]Opt{WithProviderRegistry(anthropicOnly), WithToolsetRegistry(thinkOnly)}, extra...)
}

func strictRunConfig() *config.RuntimeConfig {
	return &config.RuntimeConfig{
		EnvProviderForTests: environment.NewEnvListProvider([]string{"ANTHROPIC_API_KEY=dummy", "OPENAI_API_KEY=dummy"}),
	}
}

func TestLoadStrict_RejectsEverythingUnsupportedAtOnce(t *testing.T) {
	t.Parallel()

	_, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(strictYAML)), strictRunConfig(), strictTestOpts(WithStrict())...)
	require.Error(t, err)

	var unsupported *config.UnsupportedError
	require.ErrorAs(t, err, &unsupported)
	assert.Contains(t, err.Error(), `provider "openai" at agents.root.fallback.models[0]`)
	assert.Contains(t, err.Error(), `toolset type "shell" at agents.root.toolsets[1]`)
	assert.Contains(t, err.Error(), `feature "hooks" at agents.root.hooks`)
	assert.NotContains(t, err.Error(), `"anthropic"`)
	assert.NotContains(t, err.Error(), `"think"`)
}

func TestLoadStrict_PassesWhenEverythingIsEnabled(t *testing.T) {
	t.Parallel()

	yaml := `
models:
  main:
    provider: anthropic
    model: claude-sonnet-4-5
agents:
  root:
    model: main
    instruction: hi
    toolsets:
      - type: think
    hooks:
      pre_tool_use:
        - matcher: shell
          hooks:
            - type: command
              command: echo
`
	team, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(yaml)), strictRunConfig(), strictTestOpts(WithStrict(config.FeatureHooks))...)
	require.NoError(t, err)
	require.NotNil(t, team)
}

func TestLoadNonStrict_UnknownToolsetIsOnlyAWarning(t *testing.T) {
	t.Parallel()

	yaml := `
models:
  main:
    provider: anthropic
    model: claude-sonnet-4-5
agents:
  root:
    model: main
    instruction: hi
    toolsets:
      - type: shell
`
	team, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(yaml)), strictRunConfig(), strictTestOpts()...)
	require.NoError(t, err)
	root, err := team.Agent("root")
	require.NoError(t, err)
	warnings := root.DrainWarnings()
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "unknown toolset type: shell")
}

func TestLoadStrict_ResolvesAutoModelProvider(t *testing.T) {
	t.Parallel()

	yaml := `
agents:
  root:
    model: auto
    instruction: hi
`
	// Only OPENAI_API_KEY is set, so `auto` lands on openai, which the
	// anthropic-only registry cannot serve.
	runConfig := &config.RuntimeConfig{
		EnvProviderForTests: environment.NewEnvListProvider([]string{"OPENAI_API_KEY=dummy"}),
	}
	_, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(yaml)), runConfig, strictTestOpts(WithStrict())...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `provider "openai" at agents.root.model (auto → openai/`)
}

// External agents must be built under the same capability options as the
// parent: expander, feature wrappers, strictness.
func TestLoadExternalAgent_InheritsLoaderOptions(t *testing.T) {
	t.Parallel()

	const helperYAML = `
models:
  main:
    provider: anthropic
    model: claude-sonnet-4-5
agents:
  root:
    model: main
    instruction: helper in ${env.REGION || 'nowhere'}
    toolsets:
      - type: think
        toon: ".*"
`
	rootYAML := `
models:
  main:
    provider: anthropic
    model: claude-sonnet-4-5
agents:
  root:
    model: main
    instruction: root
    sub_agents: [myorg/helper]
`
	resolver := func(ref string, _ environment.Provider) (config.Source, error) {
		if ref != "myorg/helper" {
			return nil, fmt.Errorf("unexpected external reference %q", ref)
		}
		return config.NewBytesSource("helper.yaml", []byte(helperYAML)), nil
	}
	runConfig := &config.RuntimeConfig{
		EnvProviderForTests: environment.NewEnvListProvider([]string{"ANTHROPIC_API_KEY=dummy", "REGION=eu"}),
	}

	t.Run("without toon the external agent is rejected like the parent would be", func(t *testing.T) {
		t.Parallel()
		_, err := Load(t.Context(), config.NewBytesSource("root.yaml", []byte(rootYAML)), runConfig,
			strictTestOpts(WithSourceResolver(resolver), WithStrict(config.FeatureExternalAgents))...)
		require.ErrorContains(t, err, `feature "toon"`)
	})

	t.Run("parent options apply to the external agent", func(t *testing.T) {
		t.Parallel()
		team, err := Load(t.Context(), config.NewBytesSource("root.yaml", []byte(rootYAML)), runConfig,
			strictTestOpts(
				WithSourceResolver(resolver),
				WithExpander(js.NewJsExpander),
				WithToon(toon.Wrap),
				WithStrict(config.FeatureExternalAgents, config.FeatureToon),
			)...)
		require.NoError(t, err)
		helper, err := team.Agent("helper")
		require.NoError(t, err)
		assert.Equal(t, "helper in eu", helper.Instruction(), "JS expander must be inherited")
	})
}

func TestLoadStrict_UnsupportedProviderWinsOverMissingCredentials(t *testing.T) {
	t.Parallel()

	yaml := `
models:
  main:
    provider: openai
    model: gpt-4o
agents:
  root:
    model: main
    instruction: hi
`
	// No OPENAI_API_KEY: without strict this fails on credentials; strict
	// must report the unsupported provider first.
	runConfig := &config.RuntimeConfig{EnvProviderForTests: environment.NewEnvListProvider(nil)}
	_, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(yaml)), runConfig, strictTestOpts(WithStrict())...)
	var unsupported *config.UnsupportedError
	require.ErrorAs(t, err, &unsupported)
	assert.Contains(t, err.Error(), `provider "openai" at models.main`)
}

func TestLoadStrict_ChecksResolvedFirstAvailable(t *testing.T) {
	t.Parallel()

	yaml := `
models:
  pick:
    first_available:
      - openai/gpt-4o
      - anthropic/claude-sonnet-4-5
agents:
  root:
    model: pick
    instruction: hi
`
	t.Run("selector landing on a registered provider passes", func(t *testing.T) {
		t.Parallel()
		runConfig := &config.RuntimeConfig{EnvProviderForTests: environment.NewEnvListProvider([]string{"ANTHROPIC_API_KEY=dummy"})}
		_, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(yaml)), runConfig, strictTestOpts(WithStrict())...)
		require.NoError(t, err)
	})

	t.Run("selector landing on an unregistered provider is rejected", func(t *testing.T) {
		t.Parallel()
		runConfig := &config.RuntimeConfig{EnvProviderForTests: environment.NewEnvListProvider([]string{"OPENAI_API_KEY=dummy", "ANTHROPIC_API_KEY=dummy"})}
		_, err := Load(t.Context(), config.NewBytesSource("agent.yaml", []byte(yaml)), runConfig, strictTestOpts(WithStrict())...)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `provider "openai" at models.pick (first_available → openai/gpt-4o)`)
	})
}
