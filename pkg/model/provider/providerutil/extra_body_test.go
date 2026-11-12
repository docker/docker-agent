package providerutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMergeExtraBody(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name    string
		opts    map[string]any
		want    map[string]any
		invalid bool
	}{
		{name: "absent", want: map[string]any{"top_k": 40}},
		{name: "empty", opts: map[string]any{"extra_body": map[string]any{}}, want: map[string]any{"top_k": 40}},
		{name: "override", opts: map[string]any{"extra_body": map[string]any{"top_k": 10, "reasoning_effort": "none"}}, want: map[string]any{"top_k": 10, "reasoning_effort": "none"}},
		{name: "null", opts: map[string]any{"extra_body": nil}, invalid: true},
		{name: "string", opts: map[string]any{"extra_body": "none"}, invalid: true},
		{name: "array", opts: map[string]any{"extra_body": []any{}}, invalid: true},
		{name: "bool", opts: map[string]any{"extra_body": false}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			extras := map[string]any{"top_k": 40}
			err := MergeExtraBody(extras, tt.opts)
			if tt.invalid {
				require.EqualError(t, err, "provider_opts.extra_body must be an object")
				assert.Equal(t, map[string]any{"top_k": 40}, extras)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, extras)
		})
	}
}
