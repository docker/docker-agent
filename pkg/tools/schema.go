package tools

import (
	"encoding/json"
	"reflect"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
)

func MustSchemaFor[T any](customize ...func(*jsonschema.Schema)) any {
	schema, err := SchemaFor[T](customize...)
	if err != nil {
		panic(err)
	}
	return schema
}

func SchemaFor[T any](customize ...func(*jsonschema.Schema)) (any, error) {
	schema, err := jsonschema.For[T](&jsonschema.ForOptions{})
	if err != nil {
		return nil, err
	}
	for _, customize := range customize {
		customize(schema)
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var ordered map[string]any
	if err := json.Unmarshal(data, &ordered); err != nil {
		return nil, err
	}
	recordDeclarationOrder(ordered, reflect.TypeFor[T]())
	return schemaWithOrder(ordered), nil
}

func SchemaToMap(params any) (map[string]any, error) {
	m := map[string]any{}
	if params != nil {
		if ordered, ok := params.(schemaWithOrder); ok {
			params = map[string]any(ordered)
		}
		buf, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}

		switch params.(type) {
		case map[string]any:
			if err := json.Unmarshal(buf, &m); err != nil {
				return nil, err
			}
		default:
			var err error
			m, err = decodeSchema(buf)
			if err != nil {
				return nil, err
			}
		}
	}

	// Ensure we have at least an empty object schema.
	// That's especially important for DMR but can't hurt for others.
	if m["type"] == nil {
		m["type"] = "object"
	}
	if m["properties"] == nil {
		m["properties"] = map[string]any{}
	}
	if m["required"] == nil {
		delete(m, "required")
	}

	// Ensure all properties have a type set, recursively.
	ensurePropertyTypes(m)

	// Drop "null" from the type of required fields. Required + nullable
	// is contradictory and only inflates the schema's token cost.
	// jsonschema-go emits ["null", "array"] for any Go slice, including
	// required ones; this normalizes those back to a plain "array".
	stripNullFromRequiredTypes(m)

	return m, nil
}

// stripNullFromRequiredTypes recursively walks a JSON Schema map and removes
// "null" from the type of every property listed in its parent's "required"
// array. Optional properties are left untouched.
func stripNullFromRequiredTypes(schema map[string]any) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return
	}

	required := requiredSet(schema)

	for name, v := range props {
		prop, ok := v.(map[string]any)
		if !ok {
			continue
		}

		if required[name] {
			removeNullFromType(prop)
		}

		stripNullFromRequiredTypes(prop)
		if items, ok := prop["items"].(map[string]any); ok {
			stripNullFromRequiredTypes(items)
		}
	}
}

func requiredSet(schema map[string]any) map[string]bool {
	set := map[string]bool{}
	switch r := schema["required"].(type) {
	case []any:
		for _, name := range r {
			if s, ok := name.(string); ok {
				set[s] = true
			}
		}
	case []string:
		for _, s := range r {
			set[s] = true
		}
	}
	return set
}

func removeNullFromType(prop map[string]any) {
	// Required means the key must be present, not that its value can't be
	// null: keep "null" when an enum/const lists it as an allowed value.
	if AllowsNullValue(prop) {
		return
	}
	typeVal, exists := prop["type"]
	if !exists {
		return
	}
	arr, ok := typeVal.([]any)
	if !ok {
		return
	}

	filtered := arr[:0]
	for _, t := range arr {
		if s, ok := t.(string); ok && s == "null" {
			continue
		}
		filtered = append(filtered, t)
	}

	switch len(filtered) {
	case 0:
		// All entries were "null"; leave the schema alone.
	case 1:
		prop["type"] = filtered[0]
	default:
		prop["type"] = filtered
	}
}

// ensurePropertyTypes recursively walks a JSON Schema map and ensures
// every property has a "type" set, defaulting to "object" (or the type of
// its enum/const values, see DefaultType) if missing.
// It descends into nested "properties" and array "items".
// $ref nodes are skipped: a $ref's type is defined by its target, and
// injecting one adds a sibling keyword that providers like OpenAI reject
// on $ref nodes under strict mode.
// Composition nodes (anyOf/oneOf/allOf) are skipped for the same reason:
// their shape is the union/intersection of their variants, and a sibling
// "type" ANDs with the composition. Injecting "object" onto e.g.
// {"anyOf": [{"type":"string"}, {"type":"array"}]} (Atlassian MCP
// getConfluenceSpaces.expand, Notion MCP filters) makes the schema
// unsatisfiable; OpenAI's Responses API then returns an instant
// status=incomplete / reason=max_output_tokens with zero tokens instead of
// a 400, which surfaces as an "empty response" turn. The variants
// themselves are still walked so their own properties get typed.
func ensurePropertyTypes(schema map[string]any) {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return
	}

	for _, v := range props {
		prop, ok := v.(map[string]any)
		if !ok {
			continue
		}

		if _, isRef := prop["$ref"]; isRef {
			continue
		}

		if isCompositionSchema(prop) {
			for _, kw := range compositionKeywords {
				variants, ok := prop[kw].([]any)
				if !ok {
					continue
				}
				for _, variant := range variants {
					if vm, ok := variant.(map[string]any); ok {
						ensurePropertyTypes(vm)
						if items, ok := vm["items"].(map[string]any); ok {
							ensurePropertyTypes(items)
						}
					}
				}
			}
			continue
		}

		if prop["type"] == nil {
			prop["type"] = DefaultType(prop)
		}

		// Recurse into nested object properties.
		ensurePropertyTypes(prop)

		// Recurse into array items.
		if items, ok := prop["items"].(map[string]any); ok {
			ensurePropertyTypes(items)
		}
	}
}

// compositionKeywords are the JSON Schema keywords whose node shape is
// defined by their variants rather than by an own "type".
var compositionKeywords = []string{"anyOf", "oneOf", "allOf"}

// isCompositionSchema reports whether node uses anyOf/oneOf/allOf.
func isCompositionSchema(node map[string]any) bool {
	for _, kw := range compositionKeywords {
		if _, ok := node[kw]; ok {
			return true
		}
	}
	return false
}

func ConvertSchema(params, v any) error {
	// First unmarshal to a map to check we have a type and non-nil properties
	m, err := SchemaToMap(params)
	if err != nil {
		return err
	}

	// Then another JSON marshal/unmarshal roundtrip to the destination type
	OrderedSchemaProperties(m)
	buf, err := json.Marshal(m)
	if err != nil {
		return err
	}

	return json.Unmarshal(buf, v)
}

// DefaultType returns the "type" to give a schema node that has none: the
// JSON type(s) of its enum/const values, or "object" when it has none.
// pydantic emits a typeless enum for a mixed Literal such as
// Literal["auto", 0]; a "type": "object" sibling would reject every value.
func DefaultType(node map[string]any) any {
	var values []any
	if enum, ok := node["enum"].([]any); ok {
		values = slices.Clone(enum)
	}
	if c, ok := node["const"]; ok {
		values = append(values, c)
	}

	var types []string
	for _, v := range values {
		var t string
		switch v := v.(type) {
		case nil:
			t = "null"
		case bool:
			t = "boolean"
		case string:
			t = "string"
		case float64:
			t = "number"
			if v == float64(int64(v)) {
				t = "integer"
			}
		case int, int32, int64:
			t = "integer"
		case []any:
			t = "array"
		default:
			t = "object"
		}
		if !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	// "number" already admits integers.
	if slices.Contains(types, "number") {
		types = slices.DeleteFunc(types, func(t string) bool { return t == "integer" })
	}

	switch len(types) {
	case 0:
		return "object"
	case 1:
		return types[0]
	default:
		out := make([]any, len(types))
		for i, t := range types {
			out[i] = t
		}
		return out
	}
}

// AllowsNullValue reports whether node's enum or const explicitly lists null.
func AllowsNullValue(node map[string]any) bool {
	if enum, ok := node["enum"].([]any); ok && slices.Contains(enum, nil) {
		return true
	}
	c, ok := node["const"]
	return ok && c == nil
}
