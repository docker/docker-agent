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

// GetProviderOptStringSlice extracts a []string value from provider opts.
// YAML parses sequences as []any, so both []string and []any of strings are
// accepted. A sequence containing any non-string element is rejected.
func GetProviderOptStringSlice(opts map[string]any, key string) ([]string, bool) {
	if opts == nil {
		return nil, false
	}
	v, ok := opts[key]
	if !ok {
		return nil, false
	}
	switch s := v.(type) {
	case []string:
		return s, true
	case []any:
		out := make([]string, len(s))
		for i, e := range s {
			str, ok := e.(string)
			if !ok {
				slog.Debug("provider_opts element type mismatch, ignoring",
					"key", key,
					"expected_type", "string",
					"actual_type", fmt.Sprintf("%T", e),
					"value", e)
				return nil, false
			}
			out[i] = str
		}
		return out, true
	default:
		slog.Debug("provider_opts type mismatch, ignoring",
			"key", key,
			"expected_type", "list of strings",
			"actual_type", fmt.Sprintf("%T", v),
			"value", v)
		return nil, false
	}
}

// ExtraBody returns the provider_opts.extra_body object: arbitrary JSON
// fields the user wants merged verbatim into every chat completion request
// body, for vendor-specific parameters docker-agent has no field for (e.g.
// {"reasoning_effort": "none"} or {"thinking": {"type": "disabled"}}). Returns
// nil when unset or not an object.
func ExtraBody(opts map[string]any) map[string]any {
	v, ok := opts["extra_body"]
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		slog.Debug("provider_opts type mismatch, ignoring",
			"key", "extra_body",
			"expected_type", "object",
			"actual_type", fmt.Sprintf("%T", v),
			"value", v)
		return nil
	}
	return m
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
