package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPDefinitions_BasicRef(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/mcp_definitions.yaml"))
	require.NoError(t, err)

	root, ok := cfg.Agents.Lookup("root")
	require.True(t, ok)
	require.Len(t, root.Toolsets, 2)

	// context7 definition (docker ref) is resolved onto the toolset.
	ts0 := root.Toolsets[0]
	assert.Equal(t, "mcp", ts0.Type)
	assert.Equal(t, "docker:context7", ts0.Ref)

	// custom_mcp definition (command) is resolved onto the toolset.
	ts1 := root.Toolsets[1]
	assert.Equal(t, "mcp", ts1.Type)
	assert.Equal(t, "my-mcp-server", ts1.Command)
	assert.Equal(t, []string{"--port", "8080"}, ts1.Args)
	assert.Equal(t, map[string]string{"MY_VAR": "my_value"}, ts1.Env)
	assert.Empty(t, ts1.Ref)

	// The same definition is reusable across agents.
	other, ok := cfg.Agents.Lookup("other")
	require.True(t, ok)
	require.Len(t, other.Toolsets, 1)
	assert.Equal(t, "docker:context7", other.Toolsets[0].Ref)
}

func TestMCPDefinitions_OverrideFields(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/mcp_definitions_override.yaml"))
	require.NoError(t, err)

	root, ok := cfg.Agents.Lookup("root")
	require.True(t, ok)
	require.Len(t, root.Toolsets, 1)

	ts := root.Toolsets[0]
	assert.Equal(t, "docker:github", ts.Ref)

	// Toolset-level tools override the definition.
	assert.Equal(t, []string{"create_issue"}, ts.Tools)

	// Unset fields fall back to the definition.
	assert.Equal(t, "Use this for GitHub operations", ts.Instruction)
	assert.Equal(t, "GitHub MCP", ts.Name)

	// Env is merged: definition + toolset, toolset wins on conflicts.
	assert.Equal(t, "token123", ts.Env["GITHUB_TOKEN"])
	assert.Equal(t, "extra", ts.Env["EXTRA_VAR"])
}

func TestMCPDefinitions_InvalidRef(t *testing.T) {
	t.Parallel()

	_, err := Load(t.Context(), NewFileSource("testdata/mcp_definitions_invalid_ref.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-existent MCP definition 'nonexistent'")
}

func TestMCPDefinitions_InvalidDefinition(t *testing.T) {
	t.Parallel()

	_, err := Load(t.Context(), NewFileSource("testdata/mcp_definitions_invalid_def.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "either command, remote or ref must be set")
}

func TestMCPDefinitions_Remote(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/mcp_definitions_remote.yaml"))
	require.NoError(t, err)

	root, ok := cfg.Agents.Lookup("root")
	require.True(t, ok)
	require.Len(t, root.Toolsets, 1)

	ts := root.Toolsets[0]
	assert.Equal(t, "https://mcp.example.com/sse", ts.Remote.URL)
	assert.Equal(t, "Bearer token123", ts.Remote.Headers["Authorization"])
	assert.Empty(t, ts.Ref)
}

func TestMCPDefinitions_NoMCPsSection(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/autoregister.yaml"))
	require.NoError(t, err)
	assert.Nil(t, cfg.MCPs)
}

func TestMCPDefinitions_RejectsNonsenseFields(t *testing.T) {
	t.Parallel()

	_, err := Load(t.Context(), NewFileSource("testdata/mcp_definitions_invalid_fields.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared can only be used with type 'todo'")
}

func TestMCPDefinitions_RejectsMultipleSources(t *testing.T) {
	t.Parallel()

	_, err := Load(t.Context(), NewFileSource("testdata/mcp_definitions_multiple_sources.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "either command, remote or ref must be set, but only one of those")
}

func TestMCPDefinitions_EnvMerge(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/mcp_definitions_env_merge.yaml"))
	require.NoError(t, err)

	root, ok := cfg.Agents.Lookup("root")
	require.True(t, ok)
	require.Len(t, root.Toolsets, 1)

	ts := root.Toolsets[0]
	// Toolset value wins on key conflict
	assert.Equal(t, "from_toolset", ts.Env["KEY"])
	// Definition-only key is preserved
	assert.Equal(t, "from_definition", ts.Env["SHARED"])
	// Toolset-only key is preserved
	assert.Equal(t, "from_toolset", ts.Env["EXTRA"])
}

func TestMCPDefinitions_WorkingDir(t *testing.T) {
	t.Parallel()

	cfg, err := Load(t.Context(), NewFileSource("testdata/mcp_definitions_working_dir.yaml"))
	require.NoError(t, err)

	// WorkingDir from the definition is inherited by the referencing toolset.
	root, ok := cfg.Agents.Lookup("root")
	require.True(t, ok)
	require.Len(t, root.Toolsets, 1)
	ts := root.Toolsets[0]
	assert.Equal(t, "my-mcp-server", ts.Command)
	assert.Equal(t, "./tools/mcp", ts.WorkingDir)

	// A toolset-level working_dir overrides the definition's value.
	override, ok := cfg.Agents.Lookup("override")
	require.True(t, ok)
	require.Len(t, override.Toolsets, 1)
	tsOverride := override.Toolsets[0]
	assert.Equal(t, "./override/path", tsOverride.WorkingDir)
}
