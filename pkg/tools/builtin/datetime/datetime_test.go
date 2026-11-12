package datetime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestGetDatetimeFormats(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 7, 23, 4, 5, 123456789, time.UTC)
	tests := []struct {
		name string
		args Args
		want string
	}{
		{name: "date", args: Args{Format: "2006-01-02", Timezone: "UTC"}, want: "2026-10-07"},
		{name: "time", args: Args{Format: "15:04:05", Timezone: "UTC"}, want: "23:04:05"},
		{name: "datetime", args: Args{Format: time.RFC3339, Timezone: "UTC"}, want: "2026-10-07T23:04:05Z"},
		{name: "nanoseconds", args: Args{Format: time.RFC3339Nano, Timezone: "UTC"}, want: "2026-10-07T23:04:05.123456789Z"},
		{name: "long date", args: Args{Format: "Monday, January 2, 2006", Timezone: "UTC"}, want: "Wednesday, October 7, 2026"},
		{name: "12 hour time", args: Args{Format: "3:04 PM", Timezone: "UTC"}, want: "11:04 PM"},
		{name: "daylight saving time", args: Args{Format: time.RFC3339, Timezone: "America/New_York"}, want: "2026-10-07T19:04:05-04:00"},
		{name: "next day", args: Args{Format: time.RFC3339, Timezone: "Asia/Tokyo"}, want: "2026-10-08T08:04:05+09:00"},
		{name: "fractional offset", args: Args{Format: time.RFC3339, Timezone: "Asia/Kathmandu"}, want: "2026-10-08T04:49:05+05:45"},
		{name: "preserve whitespace", args: Args{Format: " 2006-01-02\n", Timezone: "UTC"}, want: " 2026-10-07\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := &ToolSet{now: func() time.Time { return now }}
			result, err := ts.get(t.Context(), tt.args)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.False(t, result.IsError)
			assert.Equal(t, tt.want, result.Output)
		})
	}
}

func TestGetDatetimeInvalidArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args Args
		want string
	}{
		{name: "missing format", want: "format is required"},
		{name: "blank format", args: Args{Format: " \t\n"}, want: "format is required"},
		{name: "invalid timezone", args: Args{Format: time.RFC3339, Timezone: "Not/A_Zone"}, want: `invalid timezone "Not/A_Zone"`},
		{name: "traversal", args: Args{Format: time.RFC3339, Timezone: "../etc/passwd"}, want: "invalid timezone"},
		{name: "absolute path", args: Args{Format: time.RFC3339, Timezone: "/etc/passwd"}, want: "invalid timezone"},
		{name: "null byte", args: Args{Format: time.RFC3339, Timezone: "UTC\x00"}, want: "invalid timezone"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := New().get(t.Context(), tt.args)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.True(t, result.IsError)
			assert.Contains(t, result.Output, tt.want)
		})
	}
}

func TestGetDatetimeReadsClockOnEachCall(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.October, 7, 23, 4, 5, 0, time.UTC)
	calls := 0
	ts := New()
	require.NotNil(t, ts.now)
	ts.now = func() time.Time {
		calls++
		return now
	}
	for range 2 {
		result, err := ts.get(t.Context(), Args{Format: time.RFC3339Nano, Timezone: "UTC"})
		require.NoError(t, err)
		require.False(t, result.IsError)
		assert.Equal(t, now.Format(time.RFC3339Nano), result.Output)
		now = now.Add(time.Hour)
	}
	assert.Equal(t, 2, calls)
}

func TestDatetimeToolDefinitionAndHandler(t *testing.T) {
	t.Parallel()

	ts, err := CreateToolSet()
	require.NoError(t, err)
	list, err := ts.Tools(t.Context())
	require.NoError(t, err)
	require.Len(t, list, 1)
	tool := list[0]
	assert.Equal(t, ToolNameGetDatetime, tool.Name)
	assert.Equal(t, "datetime", tool.Category)
	assert.True(t, tool.Annotations.ReadOnlyHint)
	assert.True(t, tool.AllowRepeatedCalls)
	assert.False(t, tool.Annotations.IdempotentHint)
	assert.NotEmpty(t, tools.GetInstructions(ts))

	params, err := tools.SchemaToMap(tool.Parameters)
	require.NoError(t, err)
	assert.Equal(t, "object", params["type"])
	assert.Equal(t, []any{"format"}, params["required"])
	properties := params["properties"].(map[string]any)
	assert.Contains(t, properties, "format")
	assert.Contains(t, properties, "timezone")
	output, err := tools.SchemaToMap(tool.OutputSchema)
	require.NoError(t, err)
	assert.Equal(t, "string", output["type"])

	result, err := tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{
		Name: ToolNameGetDatetime, Arguments: `{"format":"2006-01-02","timezone":"UTC"}`,
	}}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, result.Output)

	result, err = tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{
		Name: ToolNameGetDatetime, Arguments: `{}`,
	}}, tools.NopRuntime{})
	require.NoError(t, err)
	assert.True(t, result.IsError)

	_, err = tool.Handler(t.Context(), tools.ToolCall{Function: tools.FunctionCall{
		Name: ToolNameGetDatetime, Arguments: `{"format":{}}`,
	}}, tools.NopRuntime{})
	require.Error(t, err)
}
