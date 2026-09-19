package providerutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetProviderOptFloat64(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		opts   map[string]any
		key    string
		want   float64
		wantOK bool
	}{
		{"nil opts", nil, "top_k", 0, false},
		{"missing key", map[string]any{}, "top_k", 0, false},
		{"float64 value", map[string]any{"top_k": 40.0}, "top_k", 40.0, true},
		{"int value", map[string]any{"top_k": 40}, "top_k", 40.0, true},
		{"int64 value", map[string]any{"top_k": int64(40)}, "top_k", 40.0, true},
		{"float32 value", map[string]any{"top_k": float32(40.5)}, "top_k", float64(float32(40.5)), true},
		{"string value", map[string]any{"top_k": "40"}, "top_k", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := GetProviderOptFloat64(tt.opts, tt.key)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.InDelta(t, tt.want, got, 0.001)
			}
		})
	}
}

func TestGetProviderOptBool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		opts   map[string]any
		key    string
		want   bool
		wantOK bool
	}{
		{"nil opts", nil, "google_search", false, false},
		{"missing key", map[string]any{}, "google_search", false, false},
		{"true value", map[string]any{"google_search": true}, "google_search", true, true},
		{"false value", map[string]any{"google_search": false}, "google_search", false, true},
		{"string value", map[string]any{"google_search": "true"}, "google_search", false, false},
		{"int value", map[string]any{"google_search": 1}, "google_search", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := GetProviderOptBool(tt.opts, tt.key)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestGetProviderOptStringSlice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		opts   map[string]any
		key    string
		want   []string
		wantOK bool
	}{
		{"nil opts", nil, "fallbacks", nil, false},
		{"missing key", map[string]any{}, "fallbacks", nil, false},
		{"[]string value", map[string]any{"fallbacks": []string{"a", "b"}}, "fallbacks", []string{"a", "b"}, true},
		{"[]any of strings", map[string]any{"fallbacks": []any{"a", "b"}}, "fallbacks", []string{"a", "b"}, true},
		{"empty []any", map[string]any{"fallbacks": []any{}}, "fallbacks", []string{}, true},
		{"[]any with non-string", map[string]any{"fallbacks": []any{"a", 42}}, "fallbacks", nil, false},
		{"string value", map[string]any{"fallbacks": "a"}, "fallbacks", nil, false},
		{"int value", map[string]any{"fallbacks": 42}, "fallbacks", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := GetProviderOptStringSlice(tt.opts, tt.key)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestExtraBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts map[string]any
		want map[string]any
	}{
		{"nil opts", nil, nil},
		{"missing key", map[string]any{"top_k": 40}, nil},
		{
			"object",
			map[string]any{"extra_body": map[string]any{"reasoning_effort": "none", "chat_template_kwargs": map[string]any{"enable_thinking": false}}},
			map[string]any{"reasoning_effort": "none", "chat_template_kwargs": map[string]any{"enable_thinking": false}},
		},
		{"empty object", map[string]any{"extra_body": map[string]any{}}, map[string]any{}},
		{"string value", map[string]any{"extra_body": "reasoning_effort=none"}, nil},
		{"list value", map[string]any{"extra_body": []any{"a"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ExtraBody(tt.opts))
		})
	}
}

func TestGetProviderOptInt64(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		opts   map[string]any
		key    string
		want   int64
		wantOK bool
	}{
		{"nil opts", nil, "seed", 0, false},
		{"missing key", map[string]any{}, "seed", 0, false},
		{"int value", map[string]any{"seed": 42}, "seed", 42, true},
		{"int64 value", map[string]any{"seed": int64(42)}, "seed", 42, true},
		{"float64 whole number", map[string]any{"seed": 42.0}, "seed", 42, true},
		{"float64 fractional", map[string]any{"seed": 42.5}, "seed", 0, false},
		{"string value", map[string]any{"seed": "42"}, "seed", 0, false},
		{"float64 overflow", map[string]any{"seed": 1e19}, "seed", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := GetProviderOptInt64(tt.opts, tt.key)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
