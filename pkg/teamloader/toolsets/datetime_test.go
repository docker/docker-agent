package toolsets

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/datetime"
)

func TestDatetimeToolsetLoadsAndRuns(t *testing.T) {
	t.Parallel()

	registry := NewDefaultToolsetRegistry()
	require.True(t, registry.Has("datetime"))
	ts, err := registry.CreateTool(t.Context(), latest.Toolset{Type: "datetime"}, "", &config.RuntimeConfig{}, "root")
	require.NoError(t, err)
	list, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, datetime.ToolNameGetDatetime, list[0].Name)

	result, err := list[0].Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{
		Name: datetime.ToolNameGetDatetime, Arguments: `{"format":"2006-01-02T15:04:05Z07:00","timezone":"UTC"}`,
	}}, tools.NopRuntime{})
	require.NoError(t, err)
	require.False(t, result.IsError)
	_, err = time.Parse(time.RFC3339, result.Output)
	require.NoError(t, err)
}
