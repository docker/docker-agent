package toolsets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/random"
)

func TestRandomToolsetFromConfig(t *testing.T) {
	t.Parallel()

	for name, yaml := range map[string]string{
		"inline": `
agents:
  root:
    model: openai/gpt-5-mini
    toolsets:
      - type: random
`,
		"reusable": `
toolsets:
  dice:
    type: random
agents:
  root:
    model: openai/gpt-5-mini
    use_toolsets: [dice]
`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(t.Context(), config.NewBytesSource("random.yaml", []byte(yaml)))
			require.NoError(t, err)
			require.Len(t, cfg.Agents, 1)
			require.Len(t, cfg.Agents[0].Toolsets, 1)

			registry := NewDefaultToolsetRegistry()
			require.True(t, registry.Has("random"))
			ts, err := registry.CreateTool(t.Context(), cfg.Agents[0].Toolsets[0], "", &config.RuntimeConfig{}, "root")
			require.NoError(t, err)
			assert.Equal(t, "random", tools.GetName(ts))
			allTools, err := ts.Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, allTools, 1)
			assert.Equal(t, random.ToolNameRandomInt, allTools[0].Name)

			result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
				Function: tools.FunctionCall{Name: random.ToolNameRandomInt, Arguments: `{"min":4,"max":4}`},
			}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.False(t, result.IsError)
			assert.Equal(t, "4", result.Output)
		})
	}
}
