package teamloader

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestWorkspaceRejectsForkToolCreationWarnings(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "dummy")
	data := []byte(`toolsets:
  unavailable:
    type: broken
agents:
  root:
    model: openai/gpt-4o
    instruction: test
    skills:
      - name: builder
        description: test
        context: fork
        toolsets: [unavailable]
        instructions: test
`)
	registry := NewToolsetRegistry(map[string]ToolsetCreator{
		"broken": func(context.Context, latest.Toolset, string, *config.RuntimeConfig, string) (tools.ToolSet, error) {
			return nil, errors.New("fork tool unavailable")
		},
	})
	tm, err := Load(t.Context(), config.NewBytesSource("fork-warning.yaml", data), &config.RuntimeConfig{},
		withTestProviderRegistry(WithToolsetRegistry(registry))...)
	require.NoError(t, err)
	a, err := tm.DefaultAgent()
	require.NoError(t, err)
	require.NotEmpty(t, a.DrainWarnings())
	workspace, err := tm.ForWorkingDir(t.Context(), t.TempDir())
	require.ErrorContains(t, err, "fork tool unavailable")
	assert.Nil(t, workspace)
}
