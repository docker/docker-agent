package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

const flavoredConfig = `version: "13"
agents:
  root:
    model: claude
    description: base description
    instruction: base instruction
    sub_agents: [helper]
  helper:
    model: claude
    instruction: helper instruction
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
    max_tokens: 1000
flavors:
  cheap:
    models:
      claude:
        model: claude-3-5-haiku-latest
  verbose:
    agents:
      root:
        description: verbose description
  drop-max-tokens:
    models:
      claude:
        max_tokens: null
  empty:
`

func loadFlavored(t *testing.T, doc string, flavors ...string) (*latest.Config, error) {
	t.Helper()
	return Load(t.Context(), NewBytesSource("config.yaml", []byte(doc)), WithFlavors(flavors...))
}

func TestFlavorsIgnoredWhenNoneEnabled(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, flavoredConfig)
	require.NoError(t, err)

	assert.Equal(t, "claude-sonnet-4-5", cfg.Models["claude"].Model)
	assert.Equal(t, "base description", cfg.Agents.First().Description)
	assert.Len(t, cfg.Flavors, 4)
}

func TestFlavorsApplied(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, flavoredConfig, "cheap", "verbose")
	require.NoError(t, err)

	// Patched values.
	assert.Equal(t, "claude-3-5-haiku-latest", cfg.Models["claude"].Model)
	assert.Equal(t, "verbose description", cfg.Agents.First().Description)

	// Untouched siblings survive the merge.
	assert.Equal(t, "anthropic", cfg.Models["claude"].Provider)
	assert.Equal(t, "base instruction", cfg.Agents.First().Instruction)
	assert.Equal(t, []string{"helper"}, cfg.Agents.First().SubAgents)

	// Agent declaration order is preserved: root stays the default agent.
	assert.Equal(t, "root", cfg.Agents.First().Name)
}

func TestFlavorsOrderMatters(t *testing.T) {
	t.Parallel()

	config := flavoredConfig + `  expensive:
    models:
      claude:
        model: claude-opus-4-5
`

	cfg, err := loadFlavored(t, config, "cheap", "expensive")
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-5", cfg.Models["claude"].Model)

	cfg, err = loadFlavored(t, config, "expensive", "cheap")
	require.NoError(t, err)
	assert.Equal(t, "claude-3-5-haiku-latest", cfg.Models["claude"].Model)
}

func TestFlavorsNullDeletesKey(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, flavoredConfig, "drop-max-tokens")
	require.NoError(t, err)
	assert.Nil(t, cfg.Models["claude"].MaxTokens)
}

func TestFlavorsUnknownIgnored(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, flavoredConfig, "does-not-exist", "cheap")
	require.NoError(t, err)
	assert.Equal(t, "claude-3-5-haiku-latest", cfg.Models["claude"].Model)
}

func TestFlavorsEmptyIsNoOp(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, flavoredConfig, "empty")
	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-4-5", cfg.Models["claude"].Model)
}

func TestFlavorsEnabledOnConfigWithoutFlavors(t *testing.T) {
	t.Parallel()

	config := `agents:
  root:
    model: openai/gpt-5
    instruction: hello
`

	cfg, err := loadFlavored(t, config, "cheap")
	require.NoError(t, err)
	assert.Equal(t, "openai/gpt-5", cfg.Agents.First().Model)
}

func TestFlavorsMustBeMappings(t *testing.T) {
	t.Parallel()

	config := `agents:
  root:
    model: openai/gpt-5
    instruction: hello
flavors:
  bad: [not, a, mapping]
`

	_, err := loadFlavored(t, config, "bad")
	require.ErrorContains(t, err, `flavor "bad" must be a mapping`)
}

const appendConfig = `agents:
  root:
    model: openai/gpt-5
    instruction: hello
    toolsets:
      - type: think
flavors:
  with-shell:
    agents:
      root:
        toolsets+:
          - type: shell
  with-prompts:
    agents:
      root:
        add_prompt_files+:
          - extra.md
  bad-append:
    agents:
      root:
        toolsets+:
          type: shell
  scalar-append:
    agents:
      root:
        model+:
          - extra
  mapping-append:
    agents+:
      - extra
  more-instruction:
    agents:
      root:
        instruction+:
          - |
            Also: be brief.
`

func TestFlavorsAppendToArray(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, appendConfig, "with-shell")
	require.NoError(t, err)

	toolsets := cfg.Agents.First().Toolsets
	require.Len(t, toolsets, 2)
	assert.Equal(t, "think", toolsets[0].Type)
	assert.Equal(t, "shell", toolsets[1].Type)
}

func TestFlavorsAppendCreatesArray(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, appendConfig, "with-prompts")
	require.NoError(t, err)
	assert.Equal(t, []string{"extra.md"}, cfg.Agents.First().AddPromptFiles)
}

func TestFlavorsAppendRequiresSequencePatch(t *testing.T) {
	t.Parallel()

	_, err := loadFlavored(t, appendConfig, "bad-append")
	require.ErrorContains(t, err, `applying flavor "bad-append"`)
	require.ErrorContains(t, err, `append key "toolsets+": value must be a sequence`)
}

func TestFlavorsAppendPromotesScalarBase(t *testing.T) {
	t.Parallel()

	// The patch itself succeeds ("openai/gpt-5" becomes ["openai/gpt-5",
	// "extra"]); the resulting document then fails validation like any
	// hand-written config with a list where a string belongs.
	_, err := loadFlavored(t, appendConfig, "scalar-append")
	require.ErrorContains(t, err, "model")
	require.NotContains(t, err.Error(), `applying flavor`)
}

func TestFlavorsAppendRejectsMappingBase(t *testing.T) {
	t.Parallel()

	_, err := loadFlavored(t, appendConfig, "mapping-append")
	require.ErrorContains(t, err, `applying flavor "mapping-append"`)
	require.ErrorContains(t, err, `existing value for "agents" is a mapping, not a sequence`)
}

func TestFlavorsAppendToInstruction(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, appendConfig, "more-instruction")
	require.NoError(t, err)
	assert.Equal(t, "hello\n\nAlso: be brief.\n", cfg.Agents.First().Instruction)
}

const removeConfig = `agents:
  root:
    model: claude
    instruction: hello
    sub_agents: [helper, checker]
    toolsets:
      - type: think
      - type: shell
      - type: script
        shell:
          hello:
            cmd: echo hello
            description: says hello
  helper:
    model: claude
    instruction: helper
  checker:
    model: claude
    instruction: checker
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
  spare:
    provider: openai
    model: gpt-5
flavors:
  no-shell:
    agents:
      root:
        toolsets-:
          - type: shell
  no-checker:
    agents:
      root:
        sub_agents-:
          - checker
  no-spare-model:
    models-:
      - spare
  remove-absent:
    agents:
      root:
        handoffs-:
          - nobody
  bad-remove:
    agents:
      root:
        toolsets-:
          type: shell
  scalar-remove:
    agents:
      root:
        model-:
          - claude
`

func TestFlavorsRemoveFromArrayBySubsetMatch(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, removeConfig, "no-shell")
	require.NoError(t, err)

	toolsets := cfg.Agents.First().Toolsets
	require.Len(t, toolsets, 2)
	assert.Equal(t, "think", toolsets[0].Type)
	assert.Equal(t, "script", toolsets[1].Type)
}

func TestFlavorsRemoveFromArrayByValue(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, removeConfig, "no-checker")
	require.NoError(t, err)
	assert.Equal(t, []string{"helper"}, cfg.Agents.First().SubAgents)
}

func TestFlavorsRemoveFromMappingByKey(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, removeConfig, "no-spare-model")
	require.NoError(t, err)
	assert.NotContains(t, cfg.Models, "spare")
	assert.Contains(t, cfg.Models, "claude")
}

func TestFlavorsRemoveAbsentKeyIsNoOp(t *testing.T) {
	t.Parallel()

	cfg, err := loadFlavored(t, removeConfig, "remove-absent")
	require.NoError(t, err)
	assert.Empty(t, cfg.Agents.First().Handoffs)
}

func TestFlavorsRemoveRequiresSequencePatch(t *testing.T) {
	t.Parallel()

	_, err := loadFlavored(t, removeConfig, "bad-remove")
	require.ErrorContains(t, err, `applying flavor "bad-remove"`)
	require.ErrorContains(t, err, `remove key "toolsets-": value must be a sequence`)
}

func TestFlavorsRemoveRequiresSequenceOrMappingBase(t *testing.T) {
	t.Parallel()

	_, err := loadFlavored(t, removeConfig, "scalar-remove")
	require.ErrorContains(t, err, `existing value for "model" is not a sequence or mapping`)
}

func TestFlavorsRejectedOnOlderVersions(t *testing.T) {
	t.Parallel()

	config := `version: "12"
agents:
  root:
    model: openai/gpt-5
    instruction: hello
flavors:
  cheap:
    agents:
      root:
        model: openai/gpt-5-mini
`

	_, err := loadFlavored(t, config)
	require.ErrorContains(t, err, "unknown field")
	require.ErrorContains(t, err, "config version 13")
}

// A patch adding a brand-new mapping must still get the full merge treatment:
// nested `+`/`-`/null keys inside it are operators, not literal keys.
func TestFlavorsOperatorsInsideNewMapping(t *testing.T) {
	t.Parallel()

	config := `agents:
  root:
    model: openai/gpt-5
    instruction: hello
flavors:
  extra-agent:
    agents:
      extra:
        model: openai/gpt-5
        instruction: extra
        toolsets+:
          - type: shell
        sub_agents-:
          - nobody
        description:
`

	cfg, err := loadFlavored(t, config, "extra-agent")
	require.NoError(t, err)

	require.Len(t, cfg.Agents, 2)
	extra := cfg.Agents[1]
	assert.Equal(t, "extra", extra.Name)
	require.Len(t, extra.Toolsets, 1)
	assert.Equal(t, "shell", extra.Toolsets[0].Type)
	assert.Empty(t, extra.SubAgents)
	assert.Empty(t, extra.Description)
}

func TestFlavorsPreserveMultilineStrings(t *testing.T) {
	t.Parallel()

	config := "agents:\n" +
		"  root:\n" +
		"    model: openai/gpt-5\n" +
		"    instruction: |\n" +
		"      line one\n" +
		"      line two: with colon\n" +
		"flavors:\n" +
		"  rename:\n" +
		"    agents:\n" +
		"      root:\n" +
		"        description: patched\n"

	cfg, err := loadFlavored(t, config, "rename")
	require.NoError(t, err)
	assert.Equal(t, "line one\nline two: with colon\n", cfg.Agents.First().Instruction)
	assert.Equal(t, "patched", cfg.Agents.First().Description)
}

func TestFlavorsRemoveMappingEntryMustBeKeyName(t *testing.T) {
	t.Parallel()

	config := `agents:
  root:
    model: claude
    instruction: hello
models:
  claude:
    provider: anthropic
    model: claude-sonnet-4-5
flavors:
  bad:
    models-:
      - provider: anthropic
`

	_, err := loadFlavored(t, config, "bad")
	require.ErrorContains(t, err, `remove key "models-": entries must be key names when removing from a mapping`)
}
