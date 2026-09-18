package teamloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

// TestForkSkillToolsets_Resolved verifies that an inline fork skill that
// declares assistive toolsets has them resolved from the top-level toolsets
// section and exposed through the agent's skills toolset, keyed by skill name.
func TestForkSkillToolsets_Resolved(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`toolsets:
  shell_ts:
    type: shell
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills:
      - name: builder
        description: Build things in an isolated context.
        context: fork
        toolsets:
          - shell_ts
        instructions: Do the build.
`)

	team, err := Load(t.Context(), config.NewBytesSource("fork_toolsets.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	root, err := team.Agent("root")
	require.NoError(t, err)

	var skillSet *skillstool.ToolSet
	for _, ts := range root.ToolSets() {
		if s, ok := skillSetFrom(ts); ok {
			skillSet = s
			break
		}
	}
	require.NotNil(t, skillSet, "agent must expose a skills toolset")

	prepared, errResult, err := skillSet.PrepareForkSubSession(t.Context(), skillstool.RunSkillArgs{Name: "builder", Task: "go"}, tools.NopRuntime{})
	require.NoError(t, err)
	require.Nil(t, errResult)
	require.NotNil(t, prepared)
	assert.Len(t, prepared.ToolSets, 1, "fork skill must carry its resolved assistive toolset")
}

// TestForkSkillToolsets_AllowedToolsThreaded verifies that a fork skill's
// allowed_tools list is threaded through to the prepared fork (it is enforced
// against the inherited tools at runtime, see pkg/runtime).
func TestForkSkillToolsets_AllowedToolsThreaded(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills:
      - name: auditor
        description: Read-only audit in an isolated context.
        context: fork
        allowed_tools:
          - read_file
        instructions: Inspect only.
`)

	team, err := Load(t.Context(), config.NewBytesSource("fork_allowed.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	root, err := team.Agent("root")
	require.NoError(t, err)

	var skillSet *skillstool.ToolSet
	for _, ts := range root.ToolSets() {
		if s, ok := skillSetFrom(ts); ok {
			skillSet = s
			break
		}
	}
	require.NotNil(t, skillSet)

	prepared, errResult, err := skillSet.PrepareForkSubSession(t.Context(), skillstool.RunSkillArgs{Name: "auditor", Task: "go"}, tools.NopRuntime{})
	require.NoError(t, err)
	require.Nil(t, errResult)
	require.NotNil(t, prepared)
	assert.Equal(t, []string{"read_file"}, prepared.AllowedTools)
	assert.Empty(t, prepared.ToolSets)
}

// skillSetFrom unwraps a toolset to a *skillstool.ToolSet if possible.
func skillSetFrom(ts tools.ToolSet) (*skillstool.ToolSet, bool) {
	return tools.As[*skillstool.ToolSet](ts)
}

// TestForkSkillToolsets_ReadonlyAgentFiltersMutatingTools verifies that a
// readonly agent does not gain mutating tools through a fork skill's
// assistive toolset: the agent-level readonly flag is applied to the skill's
// toolset, so only read-only tools survive. Without this, a `readonly: true`
// agent could call a mutating tool (e.g. shell) inside a fork sub-session.
func TestForkSkillToolsets_ReadonlyAgentFiltersMutatingTools(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")

	data := []byte(`toolsets:
  shell_ts:
    type: shell
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    readonly: true
    skills:
      - name: builder
        description: Build things in an isolated context.
        context: fork
        toolsets:
          - shell_ts
        instructions: Do the build.
`)

	team, err := Load(t.Context(), config.NewBytesSource("fork_readonly.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)

	root, err := team.Agent("root")
	require.NoError(t, err)

	var skillSet *skillstool.ToolSet
	for _, ts := range root.ToolSets() {
		if s, ok := skillSetFrom(ts); ok {
			skillSet = s
			break
		}
	}
	require.NotNil(t, skillSet)

	prepared, errResult, err := skillSet.PrepareForkSubSession(t.Context(), skillstool.RunSkillArgs{Name: "builder", Task: "go"}, tools.NopRuntime{})
	require.NoError(t, err)
	require.Nil(t, errResult)
	require.Len(t, prepared.ToolSets, 1)

	// Start and list the skill's toolset; the mutating shell tool must be
	// filtered out by the agent-level readonly flag, leaving only the
	// read-only-hinted tools.
	ts := prepared.ToolSets[0]
	if startable, ok := tools.As[tools.Startable](ts); ok {
		require.NoError(t, startable.Start(t.Context()))
	}
	listed, err := ts.Tools(t.Context())
	require.NoError(t, err)
	for _, tool := range listed {
		assert.NotEqual(t, "shell", tool.Name, "readonly agent must not expose the mutating shell tool via a fork skill")
		assert.NotEqual(t, "run_shell_background", tool.Name, "readonly agent must not expose mutating background-shell tools via a fork skill")
	}
}
