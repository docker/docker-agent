package random

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestRandomIntBounds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		min  int64
		max  int64
	}{
		{name: "dice", min: 1, max: 6},
		{name: "negative", min: -20, max: -10},
		{name: "mixed", min: -10, max: 10},
		{name: "zero", min: 0, max: 0},
		{name: "equal positive", min: 42, max: 42},
		{name: "equal negative", min: -42, max: -42},
		{name: "minimum singleton", min: math.MinInt64, max: math.MinInt64},
		{name: "maximum singleton", min: math.MaxInt64, max: math.MaxInt64},
		{name: "near minimum", min: math.MinInt64, max: math.MinInt64 + 1},
		{name: "near maximum", min: math.MaxInt64 - 1, max: math.MaxInt64},
		{name: "large positive", min: 0, max: math.MaxInt64},
		{name: "large negative", min: math.MinInt64, max: 0},
		{name: "full range", min: math.MinInt64, max: math.MaxInt64},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ts := New()
			for range 100 {
				result, err := ts.callTool(t.Context(), Args{Min: new(tc.min), Max: new(tc.max)})
				require.NoError(t, err)
				require.False(t, result.IsError)
				value, err := strconv.ParseInt(result.Output, 10, 64)
				require.NoError(t, err)
				assert.GreaterOrEqual(t, value, tc.min)
				assert.LessOrEqual(t, value, tc.max)
			}
		})
	}
}

func TestRandomIntHandler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    string
		want    string
		isError bool
	}{
		{name: "equal bounds", args: `{"min":42,"max":42}`, want: "42"},
		{name: "explicit zero bounds", args: `{"min":0,"max":0}`, want: "0"},
		{name: "reversed bounds", args: `{"min":6,"max":1}`, want: "min must be less than or equal to max", isError: true},
		{name: "reversed extreme bounds", args: `{"min":9223372036854775807,"max":-9223372036854775808}`, want: "min must be less than or equal to max", isError: true},
		{name: "missing min", args: `{"max":6}`, want: "min and max are required", isError: true},
		{name: "missing max", args: `{"min":1}`, want: "min and max are required", isError: true},
		{name: "missing both", args: `{}`, want: "min and max are required", isError: true},
		{name: "empty arguments", want: "min and max are required", isError: true},
		{name: "null min", args: `{"min":null,"max":6}`, want: "min and max are required", isError: true},
		{name: "null max", args: `{"min":1,"max":null}`, want: "min and max are required", isError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			allTools, err := New().Tools(t.Context())
			require.NoError(t, err)
			require.Len(t, allTools, 1)
			result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
				Function: tools.FunctionCall{Name: ToolNameRandomInt, Arguments: tc.args},
			}, tools.NopRuntime{})
			require.NoError(t, err)
			assert.Equal(t, tc.isError, result.IsError)
			assert.Equal(t, tc.want, result.Output)
		})
	}
}

func TestRandomIntRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	for _, args := range []string{
		`{"min":1.5,"max":6}`,
		`{"min":1,"max":6.5}`,
		`{"min":-9223372036854775809,"max":0}`,
		`{"min":0,"max":9223372036854775808}`,
		`{"min":"one","max":6}`,
		`{"min":1,"max":`,
	} {
		t.Run(args, func(t *testing.T) {
			t.Parallel()

			allTools, err := New().Tools(t.Context())
			require.NoError(t, err)
			result, err := allTools[0].Handler(t.Context(), tools.ToolCall{
				Function: tools.FunctionCall{Name: ToolNameRandomInt, Arguments: args},
			}, tools.NopRuntime{})
			require.Error(t, err)
			assert.Nil(t, result)
		})
	}
}

func TestRandomIntToolDefinition(t *testing.T) {
	t.Parallel()

	ts, err := CreateToolSet()
	require.NoError(t, err)
	allTools, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, allTools, 1)
	tool := allTools[0]
	assert.Equal(t, ToolNameRandomInt, tool.Name)
	assert.Equal(t, "random", tool.Category)
	assert.True(t, tool.Annotations.ReadOnlyHint)
	assert.True(t, tool.AllowRepeatedCalls)
	require.NotNil(t, tool.Handler)

	params, err := tools.SchemaToMap(tool.Parameters)
	require.NoError(t, err)
	assert.Equal(t, "object", params["type"])
	assert.ElementsMatch(t, []any{"min", "max"}, params["required"])
	props, ok := params["properties"].(map[string]any)
	require.True(t, ok)
	for _, name := range []string{"min", "max"} {
		prop, ok := props[name].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "integer", prop["type"])
	}

	output, err := tools.SchemaToMap(tool.OutputSchema)
	require.NoError(t, err)
	assert.Equal(t, "integer", output["type"])
}
