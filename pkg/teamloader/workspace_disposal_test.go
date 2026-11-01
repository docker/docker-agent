package teamloader

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/filesystem"
)

type workspaceResource struct {
	closes atomic.Int32
}

func (*workspaceResource) Tools(context.Context) ([]tools.Tool, error) { return nil, nil }
func (r *workspaceResource) Close() error {
	r.closes.Add(1)
	return nil
}

type workspaceFilesystem struct {
	*filesystem.ToolSet

	closes atomic.Int32
}

func (f *workspaceFilesystem) Close() error {
	f.closes.Add(1)
	return f.ToolSet.Close()
}

func TestWorkspaceDisposesNeverStartedFilesystemAndFork(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	dir := t.TempDir()
	var files []*workspaceFilesystem
	var forks []*workspaceResource
	registry := NewToolsetRegistry(map[string]ToolsetCreator{
		"filesystem": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
			ts := &workspaceFilesystem{ToolSet: filesystem.New(dir, filesystem.WithAllowList([]string{dir}))}
			files = append(files, ts)
			return ts, nil
		},
		"resource": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
			ts := &workspaceResource{}
			forks = append(forks, ts)
			return ts, nil
		},
	})
	data := []byte(`toolsets:
  extra:
    type: resource
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    toolsets:
      - type: filesystem
    skills:
      - name: builder
        description: test
        context: fork
        toolsets: [extra]
        instructions: test
`)
	tm, err := Load(t.Context(), config.NewBytesSource("dispose.yaml", data), &config.RuntimeConfig{},
		withTestProviderRegistry(WithToolsetRegistry(registry))...)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, tm.Dispose(context.WithoutCancel(t.Context()))) })
	workspace, err := tm.ForWorkingDir(t.Context(), dir)
	require.NoError(t, err)
	require.Len(t, files, 2)
	require.Len(t, forks, 2)
	a, err := workspace.DefaultAgent()
	require.NoError(t, err)
	for _, ts := range a.ToolSets() {
		assert.False(t, ts.(*tools.StartableToolSet).IsStarted())
	}
	require.NoError(t, workspace.Dispose(t.Context()))
	require.NoError(t, workspace.Dispose(t.Context()))
	assert.EqualValues(t, 1, files[1].closes.Load())
	assert.Zero(t, files[0].closes.Load())
	assert.EqualValues(t, 1, forks[1].closes.Load())
	assert.Zero(t, forks[0].closes.Load())
}

func TestWorkspaceConstructionRollback(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	for _, failure := range []struct {
		name, tail, options string
	}{
		{name: "ordinary warning", tail: "      - type: broken\n"},
		{name: "wrapping error", tail: "      - type: resource\n        toon: all\n"},
		{name: "fork warning", options: `    skills:
      - name: builder
        description: test
        context: fork
        toolsets: [extra, unavailable]
        instructions: test
`},
	} {
		t.Run(failure.name, func(t *testing.T) {
			var created []*workspaceResource
			registry := NewToolsetRegistry(map[string]ToolsetCreator{
				"resource": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
					r := &workspaceResource{}
					created = append(created, r)
					return r, nil
				},
				"broken": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
					return nil, errors.New("unavailable")
				},
			})
			data := []byte("toolsets:\n  extra:\n    type: resource\n  unavailable:\n    type: broken\nagents:\n  root:\n    model: openai/gpt-4o\n    instruction: test\n    toolsets:\n      - type: resource\n" + failure.tail + failure.options)
			tm, err := Load(t.Context(), config.NewBytesSource("rollback.yaml", data), &config.RuntimeConfig{},
				withTestProviderRegistry(WithToolsetRegistry(registry), func(o *loadOptions) error { o.toon = nil; return nil })...)
			if failure.name == "wrapping error" {
				require.Error(t, err)
				for _, r := range created {
					assert.EqualValues(t, 1, r.closes.Load())
				}
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, tm.Dispose(context.WithoutCancel(t.Context()))) })
			original := len(created)
			workspace, err := tm.ForWorkingDir(t.Context(), t.TempDir())
			require.Error(t, err)
			assert.Nil(t, workspace)
			for _, r := range created[:original] {
				assert.Zero(t, r.closes.Load())
			}
			require.Greater(t, len(created), original)
			for _, r := range created[original:] {
				assert.EqualValues(t, 1, r.closes.Load())
			}
		})
	}
}

func TestWorkspaceRejectsPostLoadToolChanges(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	for name, change := range map[string]agent.Opt{
		"replace":    agent.WithToolSets(&workspaceResource{}),
		"additional": agent.WithAdditionalToolSets(&workspaceResource{}),
		"tools":      agent.WithTools(tools.Tool{Name: "custom"}),
	} {
		t.Run(name, func(t *testing.T) {
			tm, err := Load(t.Context(), config.NewBytesSource("invalidation.yaml", []byte("agents:\n  root:\n    model: openai/gpt-4o\n    instruction: test\n")), &config.RuntimeConfig{}, withTestProviderRegistry()...)
			require.NoError(t, err)
			a, err := tm.DefaultAgent()
			require.NoError(t, err)
			change(a)
			workspace, err := tm.ForWorkingDir(t.Context(), t.TempDir())
			require.ErrorContains(t, err, "no working directory factory")
			assert.Nil(t, workspace)
			require.NoError(t, tm.Dispose(t.Context()))
		})
	}
}
