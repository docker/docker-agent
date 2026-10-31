package teamloader

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/codemode"
)

type workspaceRecoveringTool struct {
	healthy  bool
	starts   int
	restarts int
}

func (*workspaceRecoveringTool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{Name: "recovering", Annotations: tools.ToolAnnotations{ReadOnlyHint: true}}}, nil
}
func (s *workspaceRecoveringTool) IsStarted() bool { return s.healthy }

func (s *workspaceRecoveringTool) Start(context.Context) error {
	s.starts++
	s.healthy = true
	return nil
}

func (s *workspaceRecoveringTool) Restart(context.Context) error {
	s.restarts++
	s.healthy = true
	return nil
}

func (s *workspaceRecoveringTool) Stop(context.Context) error { s.healthy = false; return nil }

func TestWorkspaceNestedOwnersRecoverUnhealthyTools(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	for _, codeMode := range []bool{false, true} {
		name := "plain"
		if codeMode {
			name = "code mode"
		}
		t.Run(name, func(t *testing.T) {
			var raw []*workspaceRecoveringTool
			registry := NewToolsetRegistry(map[string]ToolsetCreator{
				"recovering": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
					ts := &workspaceRecoveringTool{}
					raw = append(raw, ts)
					return ts, nil
				},
			})
			source := config.NewBytesSource("recovery.yaml", []byte(`agents:
  root:
    model: openai/gpt-4o
    toolsets:
      - type: recovering
        tools: [recovering]
        readonly: true
`))
			tm, err := Load(t.Context(), source, &config.RuntimeConfig{Config: config.Config{GlobalCodeMode: codeMode}},
				withTestProviderRegistry(WithToolsetRegistry(registry), WithCodeMode(codemode.Wrap))...)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, tm.Dispose(context.WithoutCancel(t.Context()))) })
			a, err := tm.DefaultAgent()
			require.NoError(t, err)
			rebuilt, err := a.ForWorkingDir(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, rebuilt.Dispose(context.WithoutCancel(t.Context()))) })
			require.Len(t, raw, 2)
			for i, toolsets := range [][]tools.ToolSet{a.ToolSets(), rebuilt.ToolSets()} {
				require.Len(t, toolsets, 1)
				owner := toolsets[0].(*tools.StartableToolSet)
				require.NoError(t, owner.Start(t.Context()))
				raw[i].healthy = false
				assert.False(t, owner.TryIsHealthy())
				require.NoError(t, owner.Start(t.Context()))
				assert.True(t, owner.TryIsHealthy())
				assert.Equal(t, 1, raw[i].starts)
				assert.Equal(t, 1, raw[i].restarts)
			}
		})
	}
}
