package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"testing"

	"github.com/docker/aijson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResultJSON(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{name: "nil", want: `null`},
		{name: "empty array", value: []string{}, want: `[]`},
		{name: "object", value: map[string]int{"count": 2}, want: `{"count":2}`},
		{name: "HTML", value: `<p>A & B</p>`, want: `"<p>A & B</p>"`},
		{name: "URL", value: "https://example.com/?a=1&b=2", want: `"https://example.com/?a=1&b=2"`},
		{name: "control characters", value: "\"quoted\"\tline\n\x00", want: `"\"quoted\"\tline\n\u0000"`},
		{name: "literal escape", value: `\u003c`, want: `"\\u003c"`},
		{name: "Unicode", value: "café 日本語 🌍\u2028\u2029", want: `"café 日本語 🌍\u2028\u2029"`},
		{name: "raw JSON", value: json.RawMessage(`{"body":"<p>A & B</p>"}`), want: `{"body":"<p>A & B</p>"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := ResultJSON(tc.value)
			assert.False(t, result.IsError)
			assert.Equal(t, tc.want, result.Output)

			original, err := json.Marshal(tc.value)
			require.NoError(t, err)
			assert.JSONEq(t, string(original), result.Output)
			assert.Equal(t, result, ResultJSONWithOptions(tc.value, JSONResultOptions{}))

			legacy := ResultJSONWithOptions(tc.value, JSONResultOptions{EscapeHTML: true})
			assert.False(t, legacy.IsError)
			assert.Equal(t, string(original), legacy.Output)
		})
	}
}

func TestResultJSON_Error(t *testing.T) {
	t.Parallel()

	for _, value := range []any{make(chan int), math.NaN(), json.RawMessage(`invalid`)} {
		_, err := json.Marshal(value)
		require.Error(t, err)

		result := ResultJSON(value)
		assert.True(t, result.IsError)
		assert.Equal(t, err.Error(), result.Output)
		assert.Equal(t, result, ResultJSONWithOptions(value, JSONResultOptions{EscapeHTML: true}))
	}
}

func TestUnmarshalToolArgumentsRepairsInput(t *testing.T) {
	t.Parallel()

	var args struct {
		Paths []string `json:"paths"`
	}
	err := UnmarshalToolArguments(t.Context(), ToolCall{
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{"paths":"only.txt"}`,
		},
	}, &args)
	require.NoError(t, err)
	assert.Equal(t, []string{"only.txt"}, args.Paths)
}

func TestUnmarshalToolArgumentsDefaultsEmptyInput(t *testing.T) {
	t.Parallel()

	var args map[string]any
	err := UnmarshalToolArguments(t.Context(), ToolCall{}, &args)
	require.NoError(t, err)
	assert.Empty(t, args)
}

func TestNewHandler_WithArguments(t *testing.T) {
	t.Parallel()

	type Args struct {
		Name string `json:"name"`
	}

	handler := NewHandler(func(_ context.Context, args Args) (*ToolCallResult, error) {
		return ResultSuccess("hello " + args.Name), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "greet",
			Arguments: `{"name":"world"}`,
		},
	}, NopRuntime{})
	require.NoError(t, err)
	assert.Equal(t, "hello world", result.Output)
}

func TestNewHandler_EmptyArguments(t *testing.T) {
	t.Parallel()

	handler := NewHandler(func(_ context.Context, _ map[string]any) (*ToolCallResult, error) {
		return ResultSuccess("ok"), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "get_memories",
			Arguments: "",
		},
	}, NopRuntime{})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Output)
}

func TestNewHandler_EmptyObjectArguments(t *testing.T) {
	t.Parallel()

	handler := NewHandler(func(_ context.Context, _ map[string]any) (*ToolCallResult, error) {
		return ResultSuccess("ok"), nil
	})

	result, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "list_things",
			Arguments: "{}",
		},
	}, NopRuntime{})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Output)
}

func TestNewHandler_InvalidArguments(t *testing.T) {
	t.Parallel()

	handler := NewHandler(func(_ context.Context, _ map[string]any) (*ToolCallResult, error) {
		return ResultSuccess("ok"), nil
	})

	_, err := handler(t.Context(), ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: FunctionCall{
			Name:      "broken",
			Arguments: `{"unterminated`,
		},
	}, NopRuntime{})
	require.Error(t, err)
}

// The next three tests pin the docker-agent-specific behavior of NewHandler.
// Repair semantics themselves are covered by github.com/docker/aijson; what
// is local to this package is (a) that NewHandler actually delegates to
// aijson, and (b) that the OnRepair hook fans out to the tool_input_repaired
// slog event with the expected fields (and stays quiet on the hot path).

// TestNewHandler_DelegatesToAijson is the wiring canary: if someone swaps
// aijson.Unmarshal back to encoding/json.Unmarshal, this test catches it.
func TestNewHandler_DelegatesToAijson(t *testing.T) {
	t.Parallel()

	type args struct {
		Paths []string `json:"paths"`
	}
	var got args
	handler := NewHandler(func(_ context.Context, a args) (*ToolCallResult, error) {
		got = a
		return ResultSuccess("ok"), nil
	})

	_, err := handler(t.Context(), ToolCall{
		Type: "function",
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{"paths":"only.txt"}`,
		},
	}, NopRuntime{})
	require.NoError(t, err)
	assert.Equal(t, []string{"only.txt"}, got.Paths)
}

func TestNewHandler_EmitsRepairTelemetry(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	type args struct {
		Paths []string `json:"paths"`
	}
	handler := NewHandler(func(_ context.Context, _ args) (*ToolCallResult, error) {
		return ResultSuccess("ok"), nil
	})

	_, err := handler(t.Context(), ToolCall{
		Type: "function",
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{"paths":"only.txt"}`,
		},
	}, NopRuntime{})
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "tool_input_repaired")
	assert.Contains(t, out, "tool=read_multiple_files")
	// Track the exported aijson constant rather than the underlying string
	// so the assertion follows the library if it ever renames the value.
	assert.Contains(t, out, string(aijson.KindWrapInArray))
}

// TestNewHandler_NoTelemetryOnValidInput pins the hot-path contract: a
// well-formed tool call must NOT emit tool_input_repaired. Without this,
// a regression in aijson's strict-first ordering would silently flood logs.
func TestNewHandler_NoTelemetryOnValidInput(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	type args struct {
		Paths []string `json:"paths"`
	}
	handler := NewHandler(func(_ context.Context, _ args) (*ToolCallResult, error) {
		return ResultSuccess("ok"), nil
	})

	_, err := handler(t.Context(), ToolCall{
		Type: "function",
		Function: FunctionCall{
			Name:      "read_multiple_files",
			Arguments: `{"paths":["a.txt","b.txt"]}`,
		},
	}, NopRuntime{})
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "tool_input_repaired")
}

func TestToolCallResultWithoutPayload(t *testing.T) {
	t.Parallel()

	result := &ToolCallResult{
		Output:            "large output",
		IsError:           true,
		Meta:              "metadata",
		Images:            []MediaContent{{Data: "image", MimeType: "image/png"}},
		Audios:            []MediaContent{{Data: "audio", MimeType: "audio/wav"}},
		Documents:         []DocumentContent{{Name: "report.pdf", MimeType: "application/pdf", Data: "pdf"}},
		StructuredContent: map[string]any{"key": "value"},
	}

	slim := result.WithoutPayload()

	require.NotNil(t, slim)
	assert.Empty(t, slim.Output)
	assert.True(t, slim.IsError)
	assert.Equal(t, "metadata", slim.Meta)
	assert.Nil(t, slim.Images)
	assert.Nil(t, slim.Audios)
	assert.Nil(t, slim.Documents)
	assert.Nil(t, slim.StructuredContent)
}
