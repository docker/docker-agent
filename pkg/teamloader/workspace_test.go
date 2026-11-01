package teamloader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	providerdefaults "github.com/docker/docker-agent/pkg/model/provider/providers"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
	"github.com/docker/docker-agent/pkg/tools/builtin/shell"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

func TestWorkspaceIsolatesShellFilesystemAndReusesModels(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	originalDir, workspaceDir, nestedDir := t.TempDir(), t.TempDir(), t.TempDir()
	var modelBuilds atomic.Int32
	providers := providerdefaults.NewDefaultRegistry()
	registry := provider.NewRegistry(map[string]provider.Factory{
		"openai": func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
			modelBuilds.Add(1)
			return providers.New(ctx, cfg, env, opts...)
		},
	})
	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: shell
      - type: filesystem
`)
	runConfig := &config.RuntimeConfig{Config: config.Config{WorkingDir: originalDir}}
	tm, err := Load(t.Context(), config.NewBytesSource("workspace.yaml", data), runConfig,
		withTestProviderRegistry(WithProviderRegistry(registry), WithToolsetRegistry(NewToolsetRegistry(map[string]ToolsetCreator{
			"shell": shell.Creator, "filesystem": filesystem.Creator,
		})))...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tm.StopToolSets(t.Context())) })
	original, err := tm.DefaultAgent()
	require.NoError(t, err)
	workspace, err := tm.ForWorkingDir(t.Context(), workspaceDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, workspace.StopToolSets(t.Context())) })
	rebuilt, err := workspace.DefaultAgent()
	require.NoError(t, err)
	nested, err := rebuilt.ForWorkingDir(t.Context(), nestedDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, nested.StopToolSets(t.Context())) })
	assert.EqualValues(t, 1, modelBuilds.Load(), "workspace copies must not rebuild provider clients")
	assert.Same(t, original.Model(t.Context()), rebuilt.Model(t.Context()))
	assert.Same(t, rebuilt.Model(t.Context()), nested.Model(t.Context()))
	assert.Equal(t, originalDir, runConfig.WorkingDir)

	for i, a := range []*agent.Agent{original, rebuilt, nested} {
		dir := []string{originalDir, workspaceDir, nestedDir}[i]
		result := callWorkspaceTool(t, a, "shell", map[string]any{"cmd": "echo shell > shell.txt"})
		require.False(t, result.IsError, result.Output)
		result = callWorkspaceTool(t, a, "write_file", map[string]any{"path": "filesystem.txt", "content": a.Name()})
		require.False(t, result.IsError, result.Output)
		content, err := os.ReadFile(filepath.Join(dir, "shell.txt"))
		require.NoError(t, err)
		assert.Contains(t, string(content), "shell")
		content, err = os.ReadFile(filepath.Join(dir, "filesystem.txt"))
		require.NoError(t, err)
		assert.Equal(t, a.Name(), string(content))
	}
	originalTools, copyTools := original.ToolSets(), rebuilt.ToolSets()
	require.Len(t, originalTools, len(copyTools))
	for i := range originalTools {
		assert.NotSame(t, originalTools[i], copyTools[i])
	}
}

func TestWorkspacePreservesToolAndSkillFilters(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	t.Setenv("DOCKER_AGENT_KIT_DIR", "")
	originalDir, workspaceDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{originalDir, workspaceDir} {
		for _, name := range []string{"allowed", "excluded"} {
			path := filepath.Join(dir, ".claude", "skills", name, "SKILL.md")
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte("---\nname: "+name+"\ndescription: test\n---\n"+dir), 0o644))
		}
	}
	data := []byte(`toolsets:
  files:
    type: filesystem
    tools: [read_file, write_file]
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    readonly: true
    toolsets:
      - type: filesystem
        tools: [read_file, write_file]
    skills:
      - allowed
      - name: fork
        description: test
        context: fork
        toolsets: [files]
        instructions: test
`)
	tm, err := Load(t.Context(), config.NewBytesSource("filtered.yaml", data),
		&config.RuntimeConfig{Config: config.Config{WorkingDir: originalDir}},
		withTestProviderRegistry(WithToolsetRegistry(NewToolsetRegistry(map[string]ToolsetCreator{"filesystem": filesystem.Creator})))...)
	require.NoError(t, err)
	rebuilt, err := tm.ForWorkingDir(t.Context(), workspaceDir)
	require.NoError(t, err)
	originalAgent, err := tm.DefaultAgent()
	require.NoError(t, err)
	workspaceAgent, err := rebuilt.DefaultAgent()
	require.NoError(t, err)
	for _, entry := range []struct {
		a   *agent.Agent
		dir string
	}{
		{a: originalAgent, dir: originalDir},
		{a: workspaceAgent, dir: workspaceDir},
	} {
		listed, err := entry.a.Tools(t.Context())
		require.NoError(t, err)
		var names []string
		for _, tool := range listed {
			names = append(names, tool.Name)
		}
		assert.Contains(t, names, "read_file")
		assert.NotContains(t, names, "write_file")
		assert.NotContains(t, names, "list_directory")
		var skillSet *skillstool.ToolSet
		for _, ts := range entry.a.ToolSets() {
			if s, ok := tools.As[*skillstool.ToolSet](ts); ok {
				skillSet = s
			}
		}
		require.NotNil(t, skillSet)
		loaded := skillSet.Skills()
		require.Len(t, loaded, 2)
		assert.Equal(t, "allowed", loaded[0].Name)
		assert.Equal(t, filepath.Join(entry.dir, ".claude", "skills", "allowed", "SKILL.md"), loaded[0].FilePath)
		assert.Equal(t, "fork", loaded[1].Name)
		prepared, failure, err := skillSet.PrepareForkSubSession(t.Context(), skillstool.RunSkillArgs{Name: "fork"}, tools.NopRuntime{})
		require.NoError(t, err)
		require.Nil(t, failure)
		require.Len(t, prepared.ToolSets, 1)
		assert.Equal(t, []string{"read_file"}, toolNames(t, prepared.ToolSets[0]))
	}
}

func TestWorkspaceRejectsToolCreationWarnings(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: broken
`)
	failure := errors.New("cannot create workspace tool")
	registry := NewToolsetRegistry(map[string]ToolsetCreator{
		"broken": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
			return nil, failure
		},
	})
	tm, err := Load(t.Context(), config.NewBytesSource("warning.yaml", data), &config.RuntimeConfig{},
		withTestProviderRegistry(WithToolsetRegistry(registry))...)
	require.NoError(t, err, "initial load still permits tool creation warnings")
	a, err := tm.DefaultAgent()
	require.NoError(t, err)
	require.NotEmpty(t, a.DrainWarnings())
	rebuilt, err := tm.ForWorkingDir(t.Context(), t.TempDir())
	require.ErrorContains(t, err, failure.Error())
	assert.Nil(t, rebuilt)
}

func TestWorkspaceConcurrentFactories(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	data := []byte(`agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: filesystem
`)
	tm, err := Load(t.Context(), config.NewBytesSource("concurrent.yaml", data), &config.RuntimeConfig{},
		withTestProviderRegistry(WithToolsetRegistry(NewToolsetRegistry(map[string]ToolsetCreator{"filesystem": filesystem.Creator})))...)
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 8 {
		dir := t.TempDir()
		wg.Go(func() {
			rebuilt, err := tm.ForWorkingDir(t.Context(), dir)
			if !assert.NoError(t, err) {
				return
			}
			assert.NoError(t, rebuilt.StopToolSets(t.Context()))
		})
	}
	wg.Wait()
}

func callWorkspaceTool(t *testing.T, a *agent.Agent, name string, args map[string]any) *tools.ToolCallResult {
	t.Helper()
	listed, err := a.Tools(t.Context())
	require.NoError(t, err)
	i := slices.IndexFunc(listed, func(tool tools.Tool) bool { return tool.Name == name })
	require.NotEqual(t, -1, i, "missing tool %s", name)
	encoded, err := json.Marshal(args)
	require.NoError(t, err)
	result, err := listed[i].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{Name: name, Arguments: string(encoded)}}, tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}
