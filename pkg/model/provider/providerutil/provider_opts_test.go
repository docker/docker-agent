package providerutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetProviderOptFloat64(t *testing.T) {
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

func TestGetProviderOptInt64(t *testing.T) {
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
