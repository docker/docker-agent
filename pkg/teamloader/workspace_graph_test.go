package teamloader

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

func TestWorkspaceLoaderRebindsEveryAgent(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: root instruction
    sub_agents: [worker]
    hooks:
      before_agent_run:
        - type: command
          command: 'echo {}'
    routing:
      allowed_agents: [worker, target]
      default_agent: worker
  worker:
    model: openai/gpt-4o-mini
    instruction: worker instruction
    handoffs: [target]
  target:
    model: openai/gpt-4o
    instruction: target instruction
    force_handoff: final
  final:
    model: openai/gpt-4o
    instruction: final instruction
`)
	tm, err := Load(t.Context(), config.NewBytesSource("graph.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)
	workspace, err := tm.ForWorkingDir(t.Context(), t.TempDir())
	require.NoError(t, err)
	root, err := workspace.Agent("root")
	require.NoError(t, err)
	worker, err := workspace.Agent("worker")
	require.NoError(t, err)
	target, err := workspace.Agent("target")
	require.NoError(t, err)
	require.Len(t, root.SubAgents(), 1)
	assert.Same(t, worker, root.SubAgents()[0])
	require.Len(t, worker.Handoffs(), 1)
	assert.Same(t, target, worker.Handoffs()[0])
	final, err := workspace.Agent("final")
	require.NoError(t, err)
	assert.Same(t, final, target.ForceHandoff())
	assert.Equal(t, []string{"worker", "target"}, root.Routing().AllowedAgents)
	for _, a := range workspace.AgentNames() {
		original, err := tm.Agent(a)
		require.NoError(t, err)
		rebuilt, err := workspace.Agent(a)
		require.NoError(t, err)
		assert.NotSame(t, original, rebuilt)
		assert.Equal(t, original.Instruction(), rebuilt.Instruction())
		assert.Same(t, original.Model(t.Context()), rebuilt.Model(t.Context()))
	}
}

func TestWorkspaceInitialSkillsDefaultToProcessDirectory(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	t.Setenv("DOCKER_AGENT_KIT_DIR", "")
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, ".claude", "skills", "local-only", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("---\nname: local-only\ndescription: test\n---\ncontent"), 0o644))
	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills: [local-only]
`)
	tm, err := Load(t.Context(), config.NewBytesSource("skills.yaml", data), &config.RuntimeConfig{}, withTestProviderRegistry()...)
	require.NoError(t, err)
	a, err := tm.DefaultAgent()
	require.NoError(t, err)
	require.Len(t, a.ToolSets(), 1)
	skillSet, ok := tools.As[*skillstool.ToolSet](a.ToolSets()[0])
	require.True(t, ok)
	require.Len(t, skillSet.Skills(), 1)
	assert.Equal(t, path, skillSet.Skills()[0].FilePath)
}
