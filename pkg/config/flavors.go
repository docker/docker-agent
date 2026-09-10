package config

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
)

// applyFlavors applies the enabled flavor patches from the document's
// top-level `flavors` section onto the raw YAML document, in the order the
// flavors were requested. Patching happens before version parsing so a patch
// can touch any part of the document. Enabled flavors that the document does
// not define are ignored with a debug log, so a set of flavors can be enabled
// globally and each config only reacts to the ones it declares.
//
// Merge semantics follow JSON Merge Patch (RFC 7386): mappings merge
// recursively, scalars and sequences replace the previous value, and an
// explicit null deletes the key. As extensions, a mapping key ending in `+`
// appends its sequence to the existing one instead of replacing it (e.g.
// `toolsets+:` adds entries to an agent's toolsets; a scalar base such as a
// string `instruction` is promoted to a one-element sequence first), and a key
// ending in `-` removes entries: from a mapping by key name, from a sequence
// by scalar equality or mapping subset-match. The `+`/`-` suffixes are
// reserved inside flavor patches; keys ending in them cannot be set literally.
// The `flavors` section itself is left in place; the latest schema carries it
// so parsing still succeeds.
func applyFlavors(ctx context.Context, data []byte, enabled []string) ([]byte, error) {
	if len(enabled) == 0 {
		return data, nil
	}

	// yaml.MapSlice keeps document order: agent declaration order is
	// meaningful (the first agent is the default root), so a plain
	// map[string]any round-trip would be a behavior change.
	var doc yaml.MapSlice
	if err := yaml.UnmarshalWithOptions(data, &doc, yaml.UseOrderedMap()); err != nil {
		return nil, fmt.Errorf("parsing config file for flavors\n%s", yaml.FormatError(err, true, true))
	}

	// Snapshot the flavors section so later patches cannot redefine the
	// flavors applied after them.
	flavorsValue, _ := lookupKey(doc, "flavors")
	flavors, _ := flavorsValue.(yaml.MapSlice)

	patched := false
	for _, name := range enabled {
		patch, found := lookupKey(flavors, name)
		if !found {
			slog.DebugContext(ctx, "Flavor not defined in config; ignoring", "flavor", name)
			continue
		}
		if patch == nil {
			continue // declared but empty: no-op patch
		}
		patchMap, ok := patch.(yaml.MapSlice)
		if !ok {
			return nil, fmt.Errorf("flavor %q must be a mapping to merge into the config", name)
		}
		result, err := mergePatch(doc, patchMap)
		if err != nil {
			return nil, fmt.Errorf("applying flavor %q: %w", name, err)
		}
		merged, ok := result.(yaml.MapSlice)
		if !ok {
			return nil, fmt.Errorf("applying flavor %q did not produce a mapping", name)
		}
		doc = merged
		patched = true
	}
	if !patched {
		return data, nil
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("marshalling config after applying flavors: %w", err)
	}
	return out, nil
}

// mergePatch merges patch into base with JSON Merge Patch (RFC 7386)
// semantics: mappings merge recursively, a null patch value deletes the key,
// and any other patch value (scalar, sequence, or mapping replacing a
// non-mapping) replaces the base value. A string key ending in `+` appends
// to the base sequence under the un-suffixed key instead of replacing it; a
// key ending in `-` removes matching entries from the base sequence or
// mapping under the un-suffixed key.
func mergePatch(base, patch any) (any, error) {
	patchMap, ok := patch.(yaml.MapSlice)
	if !ok {
		return patch, nil
	}
	baseMap, _ := base.(yaml.MapSlice)
	out := slices.Clone(baseMap)
	for _, item := range patchMap {
		if key, ok := item.Key.(string); ok && (strings.HasSuffix(key, "+") || strings.HasSuffix(key, "-")) {
			op := appendPatch
			if strings.HasSuffix(key, "-") {
				op = removePatch
			}
			var err error
			out, err = op(out, key[:len(key)-1], item.Value)
			if err != nil {
				return nil, err
			}
			continue
		}

		idx := slices.IndexFunc(out, func(existing yaml.MapItem) bool {
			return existing.Key == item.Key
		})
		switch {
		case item.Value == nil:
			if idx >= 0 {
				out = slices.Delete(out, idx, idx+1)
			}
		case idx >= 0:
			merged, err := mergePatch(out[idx].Value, item.Value)
			if err != nil {
				return nil, err
			}
			out[idx].Value = merged
		default:
			// New keys still get the full patch treatment (RFC 7386 merges
			// object values against an empty object): a freshly added mapping
			// may itself contain nulls or `+`/`-` keys to normalize.
			merged, err := mergePatch(nil, item.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, yaml.MapItem{Key: item.Key, Value: merged})
		}
	}
	return out, nil
}

// appendPatch handles a `key+` patch entry: it appends the patch sequence to
// the base sequence under key, creating the sequence when key is absent or
// null. A scalar base is promoted to a one-element sequence first, so
// `instruction+:` can extend a plain-string instruction (the agent decoder
// joins a list of strings back into one). The patch value must be a sequence.
func appendPatch(out yaml.MapSlice, key string, value any) (yaml.MapSlice, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("append key %q: value must be a sequence", key+"+")
	}
	idx := slices.IndexFunc(out, func(existing yaml.MapItem) bool {
		return existing.Key == key
	})
	if idx < 0 {
		return append(out, yaml.MapItem{Key: key, Value: items}), nil
	}
	var existing []any
	switch base := out[idx].Value.(type) {
	case nil:
	case []any:
		existing = base
	case yaml.MapSlice:
		return nil, fmt.Errorf("append key %q: existing value for %q is a mapping, not a sequence", key+"+", key)
	default:
		existing = []any{base}
	}
	out[idx].Value = append(slices.Clone(existing), items...)
	return out, nil
}

// removePatch handles a `key-` patch entry: it removes matching entries from
// the base value under key. When the base is a mapping, each item names a key
// to drop. When it is a sequence, a scalar item removes equal elements and a
// mapping item removes every element it subset-matches (all of the matcher's
// keys are present with matching values). A missing or null base is a no-op.
func removePatch(out yaml.MapSlice, key string, value any) (yaml.MapSlice, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("remove key %q: value must be a sequence", key+"-")
	}
	idx := slices.IndexFunc(out, func(existing yaml.MapItem) bool {
		return existing.Key == key
	})
	if idx < 0 {
		return out, nil
	}

	switch base := out[idx].Value.(type) {
	case nil:
		return out, nil
	case []any:
		out[idx].Value = slices.DeleteFunc(slices.Clone(base), func(elem any) bool {
			return slices.ContainsFunc(items, func(matcher any) bool {
				return matchesElement(elem, matcher)
			})
		})
	case yaml.MapSlice:
		for _, matcher := range items {
			switch matcher.(type) {
			case yaml.MapSlice, []any:
				return nil, fmt.Errorf("remove key %q: entries must be key names when removing from a mapping", key+"-")
			}
		}
		out[idx].Value = slices.DeleteFunc(slices.Clone(base), func(entry yaml.MapItem) bool {
			return slices.Contains(items, entry.Key)
		})
	default:
		return nil, fmt.Errorf("remove key %q: existing value for %q is not a sequence or mapping", key+"-", key)
	}
	return out, nil
}

// matchesElement reports whether a sequence element matches a removal
// matcher: mappings subset-match (every matcher key must be present in the
// element with a matching value, recursively), everything else compares by
// deep equality.
func matchesElement(elem, matcher any) bool {
	matcherMap, ok := matcher.(yaml.MapSlice)
	if !ok {
		return reflect.DeepEqual(elem, matcher)
	}
	elemMap, ok := elem.(yaml.MapSlice)
	if !ok {
		return false
	}
	for _, want := range matcherMap {
		idx := slices.IndexFunc(elemMap, func(have yaml.MapItem) bool {
			return have.Key == want.Key
		})
		if idx < 0 || !matchesElement(elemMap[idx].Value, want.Value) {
			return false
		}
	}
	return true
}

// lookupKey returns the value for key in an ordered mapping and whether the
// key is present.
func lookupKey(m yaml.MapSlice, key string) (any, bool) {
	for _, item := range m {
		if item.Key == key {
			return item.Value, true
		}
	}
	return nil, false
}
