package v15

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextSizeFromProviderOpts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts map[string]any
		want int64
	}{
		{"absent", map[string]any{}, 0},
		{"nil map", nil, 0},
		{"int", map[string]any{"context_size": 32768}, 32768},
		{"int64", map[string]any{"context_size": int64(65536)}, 65536},
		{"int32", map[string]any{"context_size": int32(4096)}, 4096},
		// goccy/go-yaml decodes a positive YAML integer as uint64 (see #3387).
		{"uint64", map[string]any{"context_size": uint64(262144)}, 262144},
		{"uint", map[string]any{"context_size": uint(8192)}, 8192},
		{"float64 (json)", map[string]any{"context_size": float64(8192)}, 8192},
		{"string decimal", map[string]any{"context_size": " 16384 "}, 16384},
		{"zero", map[string]any{"context_size": 0}, 0},
		{"negative", map[string]any{"context_size": int64(-1)}, 0},
		{"unparseable string", map[string]any{"context_size": "big"}, 0},
		{"wrong type", map[string]any{"context_size": true}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, ContextSizeFromProviderOpts(tt.opts))
		})
	}
}

// TestContextSizeFromProviderOpts_YAMLRoundTrip guards the real decode path:
// a context_size declared in YAML must resolve to its value. goccy/go-yaml
// produces uint64 for positive integers, which a naive int/int64 switch drops
// to 0 (the latent bug behind #3387's failed context_size workaround).
func TestContextSizeFromProviderOpts_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	var opts map[string]any
	require.NoError(t, yaml.Unmarshal([]byte("context_size: 262144\n"), &opts))
	assert.Equal(t, int64(262144), ContextSizeFromProviderOpts(opts))
}
