package providerutil

import (
	"fmt"
	"log/slog"
	"math"
)

// GetProviderOptFloat64 extracts a float64 value from provider opts.
// YAML may parse numbers as float64 or int, so this handles both.
func GetProviderOptFloat64(opts map[string]any, key string) (float64, bool) {
	if opts == nil {
		return 0, false
	}
	v, ok := opts[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		slog.Debug("provider_opts type mismatch, ignoring",
			"key", key,
			"expected_type", "numeric",
			"actual_type", fmt.Sprintf("%T", v),
			"value", v)
		return 0, false
	}
}

// GetProviderOptInt64 extracts an int64 value from provider opts.
// YAML may parse numbers as float64 or int, so this handles both.
func GetProviderOptInt64(opts map[string]any, key string) (int64, bool) {
	if opts == nil {
		return 0, false
	}
	v, ok := opts[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n == math.Trunc(n) && n >= math.MinInt64 && n <= math.MaxInt64 {
			return int64(n), true
		}
		slog.Debug("provider_opts: float64 value is not a valid integer",
			"key", key, "value", v)
		return 0, false
	default:
		slog.Debug("provider_opts type mismatch, ignoring",
			"key", key,
			"expected_type", "integer",
			"actual_type", fmt.Sprintf("%T", v),
			"value", v)
		return 0, false
	}
}

// GetProviderOptBool extracts a bool value from provider opts.
func GetProviderOptBool(opts map[string]any, key string) (bool, bool) {
	if opts == nil {
		return false, false
	}
	v, ok := opts[key]
	if !ok {
		return false, false
	}
	switch b := v.(type) {
	case bool:
		return b, true
	default:
		slog.Debug("provider_opts type mismatch, ignoring",
			"key", key,
			"expected_type", "bool",
			"actual_type", fmt.Sprintf("%T", v),
			"value", v)
		return false, false
	}
}

// samplingProviderOptsKeys lists the provider_opts keys that are
// treated as sampling parameters and forwarded to provider APIs.
// Provider-specific infrastructure keys (api_type, transport, region, etc.)
// are NOT included here.
var samplingProviderOptsKeys = []string{
	"top_k",
	"repetition_penalty",
	"seed",
	"min_p",
	"typical_p",
}

// SamplingProviderOptsKeys returns the list of provider_opts keys that are
// treated as sampling parameters and forwarded to provider APIs.
func SamplingProviderOptsKeys() []string {
	return append([]string(nil), samplingProviderOptsKeys...)
}
