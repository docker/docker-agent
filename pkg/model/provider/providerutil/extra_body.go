package providerutil

import (
	"errors"
	"maps"
)

// MergeExtraBody copies explicit request overrides into a non-nil, request-local map.
// Nested values are replaced, not merged; neither opts nor its nested maps are mutated.
func MergeExtraBody(extras, opts map[string]any) error {
	raw, exists := opts["extra_body"]
	if !exists {
		return nil
	}
	body, ok := raw.(map[string]any)
	if !ok {
		return errors.New("provider_opts.extra_body must be an object")
	}
	maps.Copy(extras, body)
	return nil
}
