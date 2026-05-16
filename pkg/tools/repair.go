package tools

import (
	"encoding/json"
	"reflect"
	"strings"
)

// repairKind identifies which shape repair was applied to a single field.
// Repairs are kept narrow and named so per-(model, tool) telemetry can be
// aggregated and so unintended repairs are obvious in logs.
type repairKind string

const (
	// repairDropNull removes a field whose value is JSON null when the field
	// type is one Go's json package would otherwise leave as a zero value.
	// In Go this is rarely needed (json.Unmarshal accepts null for slices,
	// pointers, maps, and interfaces and treats it as a no-op for primitive
	// scalars) but it stays here for symmetry with the framing in
	// https://x.com — primarily as a safety net for fields whose
	// custom UnmarshalJSON may otherwise reject null.
	repairDropNull repairKind = "drop_null"

	// repairUnwrapStringArray turns a JSON-encoded array delivered as a
	// string into a real array. Models routinely send
	//   "paths": "[\"a\",\"b\"]"
	// instead of
	//   "paths": ["a","b"]
	// The repair tries `json.Unmarshal` on the string value; if it parses to
	// an array we substitute the array. This must run BEFORE
	// repairWrapStringInArray, otherwise '["a","b"]' (a literal stringified
	// array) gets wrapped as ['["a","b"]'].
	repairUnwrapStringArray repairKind = "unwrap_string_array"

	// repairWrapObjectInArray turns a single-key object placeholder into a
	// one-element array. Models sometimes emit
	//   "paths": {"path": "foo.txt"}
	// when the schema expects ["foo.txt"]. We only fire this when the object
	// has exactly one entry whose value matches the slice's element kind, to
	// keep the repair narrow.
	repairWrapObjectInArray repairKind = "wrap_object_in_array"

	// repairWrapInArray wraps a bare scalar in a one-element array when the
	// schema expects an array of that scalar's kind. Catches the common
	//   "paths": "foo.txt"  →  ["foo.txt"]
	// failure mode.
	repairWrapInArray repairKind = "wrap_in_array"
)

// tryRepairToolArgs attempts the four shape repairs at the top level of a
// tool argument payload. It walks the destination struct's reflect.Type and
// looks for shape mismatches between each typed field and the corresponding
// raw JSON value, applying a small set of targeted fixes.
//
// The design follows validate-then-repair: callers MUST first try a strict
// json.Unmarshal and only invoke this function when that strict parse
// failed. The schema (the destination type) is the prior, and we only spend
// repair budget at the exact field paths the schema disagreed at.
//
// Returns the repaired JSON bytes, the list of repairs applied (for
// telemetry), and a boolean indicating whether any repair was applied. When
// the second value is empty / third is false the caller should surface the
// original validation error unchanged.
func tryRepairToolArgs(data []byte, paramsType reflect.Type) ([]byte, []repairKind, bool) {
	if paramsType == nil {
		return nil, nil, false
	}
	for paramsType.Kind() == reflect.Pointer {
		paramsType = paramsType.Elem()
	}
	if paramsType.Kind() != reflect.Struct {
		return nil, nil, false
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		// The payload isn't even a JSON object at the top level. Shape
		// repairs operate on object fields, so we have nothing to do.
		return nil, nil, false
	}

	// reflect.VisibleFields walks promoted fields from embedded structs,
	// matching how encoding/json marshals struct values into JSON objects:
	// fields lifted by embedding share the same top-level object, so they
	// must be inspected in the same `raw` map. Iterating
	// `paramsType.NumField()` directly would silently skip them — relevant
	// today for ReferencesArgs/RenameArgs/CallHierarchyArgs/TypeHierarchyArgs
	// which all embed PositionArgs.
	repairs := []repairKind{}
	for _, field := range reflect.VisibleFields(paramsType) {
		if field.Anonymous {
			// The embedding itself is a marker; its promoted children are
			// emitted as separate visible fields by VisibleFields.
			continue
		}
		name, ok := jsonFieldName(field)
		if !ok {
			continue
		}
		val, present := raw[name]
		if !present {
			continue
		}

		newVal, kind, repaired := repairFieldValue(val, field.Type)
		if !repaired {
			continue
		}
		if newVal == nil && kind == repairDropNull {
			delete(raw, name)
		} else {
			raw[name] = newVal
		}
		repairs = append(repairs, kind)
	}

	if len(repairs) == 0 {
		return nil, nil, false
	}

	out, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, false
	}
	return out, repairs, true
}

// jsonFieldName returns the JSON object key that a struct field marshals to,
// or false if the field is unexported / explicitly skipped via `json:"-"`.
func jsonFieldName(field reflect.StructField) (string, bool) {
	if !field.IsExported() {
		return "", false
	}
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	if tag == "" {
		return field.Name, true
	}
	name := strings.SplitN(tag, ",", 2)[0]
	if name == "" {
		return field.Name, true
	}
	return name, true
}

// repairFieldValue applies the four repairs to a single field. It returns
// the new value, the repair kind, and whether a repair fired. The function
// is intentionally conservative — when in doubt, return false and let the
// original validation error surface.
func repairFieldValue(val any, fieldType reflect.Type) (any, repairKind, bool) {
	for fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}

	// Repair 1: null-valued primitives. Slices/maps/pointers/interfaces
	// already accept null in Go's json package, so we only nudge for scalar
	// kinds where a custom UnmarshalJSON might object.
	if val == nil {
		switch fieldType.Kind() {
		case reflect.String, reflect.Bool,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			return nil, repairDropNull, true
		default:
			return nil, "", false
		}
	}

	// Remaining repairs all target slice fields. Anything else is left
	// alone — we explicitly do not generalise to maps or nested structs in
	// this layer. Recursion would expand the blast radius without evidence
	// it is needed.
	if fieldType.Kind() != reflect.Slice {
		return nil, "", false
	}

	// If the value already arrived as an array there is nothing to fix at
	// this level — even if individual elements are wrong, the schema can
	// surface that error itself.
	if _, ok := val.([]any); ok {
		return nil, "", false
	}

	elemType := fieldType.Elem()
	for elemType.Kind() == reflect.Pointer {
		elemType = elemType.Elem()
	}
	elemKind := elemType.Kind()

	// Repair 2: stringified JSON array. Try this BEFORE the bare-string
	// wrap, otherwise a stringified array would be wrapped as a single
	// element and we would silently corrupt the input.
	if s, ok := val.(string); ok {
		trimmed := strings.TrimSpace(s)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			var arr []any
			if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
				return arr, repairUnwrapStringArray, true
			}
		}
		// Repair 4: bare scalar where the schema expects an array of that
		// scalar's kind. Only fire for primitive-element slices to avoid
		// guessing how to construct a struct from a string.
		if isScalarKind(elemKind) {
			return []any{s}, repairWrapInArray, true
		}
		return nil, "", false
	}

	// Repair 3: object placeholder. Models sometimes emit
	//   {"paths": {"path": "foo"}}  → ["foo"]
	// We accept exactly the narrow case of a single-entry object whose
	// value is a scalar matching the slice's element kind.
	if obj, ok := val.(map[string]any); ok {
		if len(obj) != 1 {
			return nil, "", false
		}
		for _, v := range obj {
			if isScalarKind(elemKind) && matchesScalarKind(v, elemKind) {
				return []any{v}, repairWrapObjectInArray, true
			}
		}
		return nil, "", false
	}

	// Bare scalar of a different type (number, bool) where an array is
	// expected. Wrap when element kinds line up.
	if matchesScalarKind(val, elemKind) {
		return []any{val}, repairWrapInArray, true
	}

	return nil, "", false
}

// isScalarKind reports whether k is one of the primitive JSON-compatible
// reflect kinds the repair layer is willing to wrap into a one-element array.
func isScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// matchesScalarKind reports whether v (a value parsed from JSON via
// map[string]any) is compatible with the given scalar reflect.Kind. JSON
// numbers always parse to float64, so any numeric kind matches a float64.
func matchesScalarKind(v any, k reflect.Kind) bool {
	switch v.(type) {
	case string:
		return k == reflect.String
	case bool:
		return k == reflect.Bool
	case float64:
		switch k {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			return true
		}
	}
	return false
}
