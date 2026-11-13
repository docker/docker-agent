package hooks

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPipelineResultMetadataComposesIndependentlyOfText(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	require.NoError(t, registry.RegisterBuiltin("metadata", func(_ context.Context, _ *Input, _ []string) (*Output, error) {
		meta := json.RawMessage(`{"context":"redacted"}`)
		return &Output{HookSpecificOutput: &HookSpecificOutput{UpdatedToolResultMeta: meta}}, nil
	}))
	require.NoError(t, registry.RegisterBuiltin("text", func(_ context.Context, in *Input, _ []string) (*Output, error) {
		meta, err := json.Marshal(in.ToolResultMeta)
		require.NoError(t, err)
		assert.JSONEq(t, `{"context":"redacted"}`, string(meta))
		return &Output{HookSpecificOutput: &HookSpecificOutput{UpdatedToolResponse: new("rewritten")}}, nil
	}))
	executor := NewExecutorWithRegistry(&Config{ToolResponseTransform: []MatcherConfig{{Hooks: []Hook{
		{Type: HookTypeBuiltin, Command: "metadata"},
		{Type: HookTypeBuiltin, Command: "text"},
	}}}}, t.TempDir(), nil, registry)
	input := &Input{ToolResponse: "original", ToolResultMeta: map[string]any{"context": "secret"}}
	result, err := executor.Dispatch(t.Context(), EventToolResponseTransform, input)
	require.NoError(t, err)
	require.NotNil(t, result.UpdatedToolResultMeta)
	assert.JSONEq(t, `{"context":"redacted"}`, string(result.UpdatedToolResultMeta))
	require.NotNil(t, result.UpdatedToolResponse)
	assert.Equal(t, "rewritten", *result.UpdatedToolResponse)
	assert.Equal(t, map[string]any{"context": "secret"}, input.ToolResultMeta)
}
