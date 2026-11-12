package builtins_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

func TestTransformJSON(t *testing.T) {
	t.Parallel()
	fn := lookup(t, builtins.TransformJSON)
	for _, tc := range []struct {
		name, raw, want string
		args            []string
	}{
		{"selected fields", `{"name":"squirtle","types":[{"slot":1,"type":{"name":"water"}}],"stats":[{"base_stat":44}],"sprites":{}}`, `{"name":"squirtle","stats":[{"base_stat":44}],"types":[{"slot":1,"type":{"name":"water"}}]}`, []string{"name", "types", "stats"}},
		{"missing fields", `{"name":"squirtle"}`, `{"name":"squirtle"}`, []string{"name", "stats"}},
		{"no matching fields", `{"id":7}`, `{}`, []string{"name"}},
		{"empty values", `{"null":null,"array":[],"object":{},"string":""}`, `{"array":[],"null":null,"object":{},"string":""}`, []string{"null", "array", "object", "string"}},
		{"numbers unchanged", `{"id":9007199254740993,"stat":44.0}`, `{"id":9007199254740993,"stat":44.0}`, []string{"id", "stat"}},
		{"literal keys", `{"a.b":1,"other":2}`, `{"a.b":1}`, []string{"a.b"}},
		{"duplicate selection", `{"name":"squirtle"}`, `{"name":"squirtle"}`, []string{"name", "name"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := fn(t.Context(), &hooks.Input{HookEventName: hooks.EventToolResponseTransform, ToolResponse: tc.raw}, tc.args)
			require.NoError(t, err)
			require.NotNil(t, out)
			assert.Equal(t, hooks.EventToolResponseTransform, out.HookSpecificOutput.HookEventName)
			assert.Equal(t, tc.want, *out.HookSpecificOutput.UpdatedToolResponse)
		})
	}
}

func TestTransformJSONErrors(t *testing.T) {
	t.Parallel()
	fn := lookup(t, builtins.TransformJSON)
	for _, raw := range []any{nil, 42, "truncated", "null", "[]", `"text"`, `{} {}`} {
		out, err := fn(t.Context(), &hooks.Input{HookEventName: hooks.EventToolResponseTransform, ToolResponse: raw}, []string{"name"})
		require.Error(t, err)
		assert.Nil(t, out)
	}
}

func TestTransformJSONNoop(t *testing.T) {
	t.Parallel()
	fn := lookup(t, builtins.TransformJSON)
	for _, in := range []*hooks.Input{nil, {}, {HookEventName: hooks.EventPostToolUse}, {HookEventName: hooks.EventToolResponseTransform, ToolError: true}} {
		out, err := fn(t.Context(), in, []string{"name"})
		require.NoError(t, err)
		assert.Nil(t, out)
	}
	out, err := fn(t.Context(), &hooks.Input{HookEventName: hooks.EventToolResponseTransform}, nil)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestTransformJSONPipeline(t *testing.T) {
	t.Parallel()
	r := hooks.NewRegistry()
	require.NoError(t, builtins.Register(r))
	cfg := &hooks.Config{ToolResponseTransform: []hooks.MatcherConfig{{Matcher: "pokemon_retrieve", Hooks: []hooks.Hook{
		{Type: hooks.HookTypeBuiltin, Command: builtins.TransformJSON, Args: []string{"name", "stats"}},
		{Type: hooks.HookTypeBuiltin, Command: builtins.TransformJSON, Args: []string{"name"}},
	}}}}
	exec := hooks.NewExecutorWithRegistry(cfg, "", nil, r)
	in := &hooks.Input{ToolName: "pokemon_retrieve", ToolResponse: `{"name":"squirtle","stats":[],"unused":true}`}
	result, err := exec.Dispatch(t.Context(), hooks.EventToolResponseTransform, in)
	require.NoError(t, err)
	require.NotNil(t, result.UpdatedToolResponse)
	assert.JSONEq(t, `{"name":"squirtle"}`, *result.UpdatedToolResponse)
	in.ToolName = "other"
	result, err = exec.Dispatch(t.Context(), hooks.EventToolResponseTransform, in)
	require.NoError(t, err)
	assert.Nil(t, result.UpdatedToolResponse)
	in.ToolName, in.ToolResponse = "pokemon_retrieve", "truncated"
	result, err = exec.Dispatch(t.Context(), hooks.EventToolResponseTransform, in)
	require.NoError(t, err)
	assert.Nil(t, result.UpdatedToolResponse)
	assert.Equal(t, "truncated", in.ToolResponse)
}
