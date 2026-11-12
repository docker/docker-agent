package toolsets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/tools"
)

func TestCalculatorExample(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(t.Context(), config.NewFileSource("../../../examples/calculator.yaml"))
	require.NoError(t, err)
	require.Len(t, cfg.Agents, 1)
	require.Len(t, cfg.Agents[0].Toolsets, 1)
	ts, err := NewDefaultToolsetRegistry().CreateTool(t.Context(), cfg.Agents[0].Toolsets[0], "", nil, "root")
	require.NoError(t, err)
	list, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "calculate", list[0].Name)
	result, err := list[0].Handler(t.Context(), tools.ToolCall{
		Function: tools.FunctionCall{Name: "calculate", Arguments: `{"expression":"0.1 + 0.2"}`},
	}, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Output)
	assert.JSONEq(t, `{"result":"0.3","decimal":"0.3","approximate":false}`, result.Output)
}
