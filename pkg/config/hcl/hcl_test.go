package hcl

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToYAML_Pirate(t *testing.T) {
	t.Parallel()

	src := []byte(`
agent "root" {
  description = "An agent that talks like a pirate"
  instruction = "Always answer by talking like a pirate."
  model       = "auto"

  welcome_message = <<-EOT
  Ahoy! I be yer pirate guide, ready to set sail on the seas o' knowledge!
  EOT
}
`)

	out, err := ToYAML(src, "pirate.hcl")
	require.NoError(t, err)

	got := string(out)
	assert.Contains(t, got, "agents:")
	assert.Contains(t, got, "  root:")
	assert.Contains(t, got, "    description: An agent that talks like a pirate")
	assert.Contains(t, got, "    instruction: Always answer by talking like a pirate.")
	assert.Contains(t, got, "    model: auto")
	assert.Contains(t, got, "    welcome_message: ")
	assert.Contains(t, got, "Ahoy!")
}

func TestToYAML_LabeledBlocksBecomeKeyedMaps(t *testing.T) {
	t.Parallel()

	src := []byte(`
model "claude" {
  provider = "anthropic"
  model    = "claude-opus-4-6"
}

model "haiku" {
  provider = "anthropic"
  model    = "claude-haiku-4-5"
}

agent "root" {
  model       = "claude"
  instruction = "Test"
}
`)

	m, err := ToMap(src, "test.hcl")
	require.NoError(t, err)

	assert.NotNil(t, m["models"])
	assert.NotNil(t, m["agents"])
}

func TestToYAML_ToolsetLabelBecomesType(t *testing.T) {
	t.Parallel()

	src := []byte(`
agent "root" {
  instruction = "x"
  model       = "auto"

  toolset "filesystem" {}
  toolset "shell" {}
  toolset "mcp" {
    command = "gopls"
    args    = ["mcp"]
  }
}
`)

	out, err := ToYAML(src, "test.hcl")
	require.NoError(t, err)
	got := string(out)

	assert.Contains(t, got, "toolsets:")
	assert.Contains(t, got, "type: filesystem")
	assert.Contains(t, got, "type: shell")
	assert.Contains(t, got, "type: mcp")
	assert.Contains(t, got, "command: gopls")
	assert.Contains(t, got, "- mcp")
}

func TestToYAML_PreservesAgentDeclarationOrder(t *testing.T) {
	t.Parallel()

	src := []byte(`
agent "root" {
  instruction = "x"
  model       = "auto"
}
agent "planner" {
  instruction = "y"
  model       = "auto"
}
agent "reviewer" {
  instruction = "z"
  model       = "auto"
}
`)

	out, err := ToYAML(src, "test.hcl")
	require.NoError(t, err)
	got := string(out)

	rootIdx := strings.Index(got, "root:")
	plannerIdx := strings.Index(got, "planner:")
	reviewerIdx := strings.Index(got, "reviewer:")

	require.NotEqual(t, -1, rootIdx)
	require.NotEqual(t, -1, plannerIdx)
	require.NotEqual(t, -1, reviewerIdx)
	assert.Less(t, rootIdx, plannerIdx, "root should come before planner")
	assert.Less(t, plannerIdx, reviewerIdx, "planner should come before reviewer")
}

func TestToYAML_CommandShortcutOnlySupportedAsBlock(t *testing.T) {
	t.Parallel()

	src := []byte(`
agent "root" {
  instruction = "x"
  model       = "auto"

  command "fix" {
    instruction = "Fix the lint"
  }
  command "init" {
    description = "Initialize"
    instruction = "Set things up"
  }
}
`)

	out, err := ToYAML(src, "test.hcl")
	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, "commands:")
	assert.Contains(t, got, "fix:")
	assert.Contains(t, got, "init:")
	assert.Contains(t, got, "Fix the lint")
}

func TestToYAML_PermissionsSingleton(t *testing.T) {
	t.Parallel()

	src := []byte(`
agent "root" {
  instruction = "x"
  model       = "auto"
}

permissions {
  allow = ["a", "b"]
  deny  = ["c"]
}
`)

	out, err := ToYAML(src, "test.hcl")
	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, "permissions:")
	assert.Contains(t, got, "  allow:")
	assert.Contains(t, got, "  - a")
}

func TestToYAML_DuplicateLabeledBlock(t *testing.T) {
	t.Parallel()

	src := []byte(`
agent "root" {
  instruction = "x"
  model       = "auto"
}
agent "root" {
  instruction = "y"
  model       = "auto"
}
`)

	_, err := ToYAML(src, "test.hcl")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Duplicate")
}

func TestToYAML_EscapedInterpolation(t *testing.T) {
	t.Parallel()

	src := []byte(`
agent "root" {
  instruction = "$${shell()}"
  model       = "auto"
}
`)

	m, err := ToMap(src, "test.hcl")
	require.NoError(t, err)

	agents := m["agents"]
	require.NotNil(t, agents)

	// Walk through the yaml.MapSlice the converter produces to find the
	// instruction value we wrote.
	items, ok := agents.(yaml.MapSlice)
	require.True(t, ok, "agents should be a yaml.MapSlice, got %T", agents)
	require.Len(t, items, 1)
	root, ok := items[0].Value.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "${shell()}", root["instruction"], "escaped $${...} should decode to literal ${...}")
}

func TestLooksLikeHCL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data string
		want bool
	}{
		{"empty", "", false},
		{"yaml", "agents:\n  root:\n    instruction: hi\n", false},
		{"yaml with comment", "# a comment\nagents:\n  root: {}\n", false},
		{"yaml quoted key", "agent \"root\":\n  instruction: hi\n", false},
		{"hcl with shebang", "#!/usr/bin/env docker agent run\n\nagent \"root\" {\n  model = \"auto\"\n}\n", true},
		{"hcl permissions block", "permissions {\n  allow = [\"a\"]\n}\n", true},
		{"hcl with line comment", "// a comment\nagent \"root\" {}\n", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := LooksLikeHCL([]byte(tc.data))
			assert.Equal(t, tc.want, got)
		})
	}
}
