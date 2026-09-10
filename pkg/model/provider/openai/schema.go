package openai

import (
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/openai/openai-go/v3/shared"

	"github.com/docker/docker-agent/pkg/tools"
)

// ConvertParametersToSchema converts parameters to OpenAI Schema format and
// reports whether the resulting schema is compatible with OpenAI strict mode.
//
// The same normalization pipeline runs in both cases — strict-incompatible
// schemas (e.g. Notion MCP tools that declare schema-form additionalProperties)
// still need fully-populated `required` arrays for the Chat Completions API,
// which has no per-tool strict flag. The strict flag is only consumed by the
// Responses API caller.
func ConvertParametersToSchema(params any) (shared.FunctionParameters, bool, error) {
	p, err := tools.SchemaToMap(params)
	if err != nil {
		return nil, false, err
	}

	strict := isStrictCompatible(p)
	return fixSchemaArrayItems(removeFormatFields(ensureTypeFields(makeAllRequired(p)))), strict, nil
}

// childSchemas yields every direct sub-schema of node. Both the strict
// compatibility check and the normalization walker use it so they cannot
// drift. $ref is deliberately not followed: every local target lives in a
// container walked here (or is the root), and following refs would loop on
// recursive schemas.
func childSchemas(node map[string]any) iter.Seq[map[string]any] {
	return func(yield func(map[string]any) bool) {
		for _, key := range []string{"properties", "patternProperties", "$defs", "definitions"} {
			if m, ok := node[key].(map[string]any); ok {
				for _, v := range m {
					if sub, ok := v.(map[string]any); ok && !yield(sub) {
						return
					}
				}
			}
		}

		for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
			if arr, ok := node[key].([]any); ok {
				for _, v := range arr {
					if sub, ok := v.(map[string]any); ok && !yield(sub) {
						return
					}
				}
			}
		}

		for _, key := range []string{"items", "additionalProperties", "not", "if", "then", "else", "contains", "propertyNames"} {
			if sub, ok := node[key].(map[string]any); ok && !yield(sub) {
				return
			}
		}
	}
}

// strictUnsupportedKeywords lists keywords OpenAI Structured Outputs rejects
// regardless of their content. oneOf is absent from OpenAI's supported-type
// list and is rejected in practice (#4106); the rest are documented as
// unsupported composition keywords.
var strictUnsupportedKeywords = []string{
	"oneOf", "allOf", "not", "if", "then", "else", "dependentRequired", "dependentSchemas",
}

// isStrictCompatible reports whether the schema can use OpenAI strict mode.
// Strict mode requires every object node to have additionalProperties: false,
// forbids composition keywords other than anyOf, and only allows $ref nodes
// that are local, resolvable, and free of sibling keywords.
//
// The decision is per-tool and all-or-nothing: a single non-compliant node
// anywhere in the schema disables strict mode for the whole tool. The walk
// stops at the first incompatible node.
func isStrictCompatible(schema map[string]any) bool {
	return !hasIncompatibleNode(schema, schema)
}

func hasIncompatibleNode(root, node map[string]any) bool {
	for _, kw := range strictUnsupportedKeywords {
		if _, ok := node[kw]; ok {
			return true
		}
	}

	if v, ok := node["additionalProperties"]; ok {
		switch t := v.(type) {
		case map[string]any:
			return true
		case bool:
			if t {
				return true
			}
		}
	}

	if ref, ok := node["$ref"]; ok && !isStrictCompatibleRef(root, node, ref) {
		return true
	}

	for sub := range childSchemas(node) {
		if hasIncompatibleNode(root, sub) {
			return true
		}
	}
	return false
}

// isStrictCompatibleRef reports whether a $ref node is compatible with
// OpenAI strict mode: the ref must be a string, a local JSON pointer ("#" or
// "#/...") that resolves within root, and the node must have no sibling
// keywords.
//
// The no-siblings rule follows the official openai-python SDK
// (_ensure_strict_json_schema), which inlines any $ref carrying sibling keys
// because OpenAI rejects e.g. {"$ref": "...", "description": "..."} — see
// openai-python#1631. We mark such nodes non-strict instead of inlining,
// since $defs/$ref are documented as supported and inlining would break
// recursive schemas.
func isStrictCompatibleRef(root, node map[string]any, ref any) bool {
	refStr, ok := ref.(string)
	if !ok {
		return false
	}
	if refStr != "#" && !strings.HasPrefix(refStr, "#/") {
		return false
	}
	if _, ok := resolveJSONPointer(root, refStr); !ok {
		return false
	}
	// No sibling keywords: the node must contain nothing but "$ref".
	return len(node) == 1
}

// resolveJSONPointer resolves a local JSON pointer (e.g. "#/$defs/thing" or
// "#") against root, per RFC 6901. It only descends through map[string]any
// and []any nodes; anything else, or a missing key/out-of-range index, fails
// resolution rather than panicking.
func resolveJSONPointer(root map[string]any, pointer string) (map[string]any, bool) {
	pointer = strings.TrimPrefix(pointer, "#")
	if pointer == "" {
		return root, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}

	var current any = root
	for tok := range strings.SplitSeq(pointer[1:], "/") {
		tok = strings.ReplaceAll(tok, "~1", "/")
		tok = strings.ReplaceAll(tok, "~0", "~")

		switch node := current.(type) {
		case map[string]any:
			v, ok := node[tok]
			if !ok {
				return nil, false
			}
			current = v
		case []any:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			current = node[idx]
		default:
			return nil, false
		}
	}

	m, ok := current.(map[string]any)
	return m, ok
}

// walkSchema calls fn on the given schema node, then recursively walks into
// every sub-schema childSchemas yields (properties, patternProperties,
// $defs/definitions, anyOf/oneOf/allOf/prefixItems variants, items,
// additionalProperties, and the remaining single-schema keywords). $ref is
// never followed, so recursive schemas terminate.
func walkSchema(schema map[string]any, fn func(map[string]any)) {
	fn(schema)
	for sub := range childSchemas(schema) {
		walkSchema(sub, fn)
	}
}

// makeAllRequired makes every object property `required` (newly-required ones
// are made nullable) and ensures every object node has `additionalProperties`
// set. It runs on every schema regardless of strict-mode compatibility, so
// schema-form additionalProperties (e.g. Notion's dictionary value shape) is
// preserved — only missing/true/nil values are forced to `false`.
//
// $ref nodes are left untouched (no additionalProperties/type injection): a
// newly-required $ref property is instead wrapped as
// {"anyOf": [<$ref>, {"type": "null"}]}, OpenAI's documented pattern for an
// optional reference, so the model isn't forced to always emit it.
func makeAllRequired(schema shared.FunctionParameters) shared.FunctionParameters {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}

	walkSchema(schema, func(node map[string]any) {
		if _, ok := node["$ref"]; ok {
			return
		}

		isObject := false
		if typeVal, ok := node["type"]; ok {
			switch t := typeVal.(type) {
			case string:
				isObject = t == "object"
			case []any:
				for _, v := range t {
					if s, ok := v.(string); ok && s == "object" {
						isObject = true
						break
					}
				}
			case []string:
				isObject = slices.Contains(t, "object")
			}
		}

		// Only force additionalProperties: false when it isn't already a
		// schema. Schema-form additionalProperties carries information the
		// model needs (Notion-style dictionaries) and would be lost otherwise.
		if isObject {
			if addProps, exists := node["additionalProperties"]; !exists || addProps == nil || addProps == true {
				node["additionalProperties"] = false
			}
		}

		properties, ok := node["properties"].(map[string]any)
		if !ok {
			return
		}

		originallyRequired := map[string]bool{}
		if required, ok := node["required"].([]any); ok {
			for _, name := range required {
				originallyRequired[name.(string)] = true
			}
		}

		newRequired := []any{}
		for _, propName := range slices.Sorted(maps.Keys(properties)) {
			newRequired = append(newRequired, propName)
			if !originallyRequired[propName] {
				if propMap, ok := properties[propName].(map[string]any); ok {
					if _, isRef := propMap["$ref"]; isRef {
						properties[propName] = map[string]any{
							"anyOf": []any{propMap, map[string]any{"type": "null"}},
						}
					} else if t, ok := propMap["type"].(string); ok {
						propMap["type"] = []string{t, "null"}
					}
				}
			}
		}

		node["required"] = newRequired
	})

	return schema
}

// isCompositionNode reports whether node describes its shape via $ref or a
// composition keyword (anyOf/oneOf/allOf) rather than its own "type". A
// "type" sibling combines with these via AND, so injecting one is only safe
// when every variant already shares it — otherwise the schema becomes
// unsatisfiable (e.g. a newly-required $ref property wrapped as
// {"anyOf": [<$ref>, {"type": "null"}]} would gain a conflicting
// "type": "object" and could never validate as null, or as anything at all
// if the ref target isn't itself an object). Nodes like this are left
// without an injected type; OpenAI's own examples show anyOf/$ref nodes
// with no sibling type.
func isCompositionNode(node map[string]any) bool {
	for _, kw := range []string{"$ref", "anyOf", "oneOf", "allOf"} {
		if _, ok := node[kw]; ok {
			return true
		}
	}
	return false
}

// ensureTypeFields ensures every schema node that is a map has a "type" key.
// OpenAI Responses API requires all schema nodes to have an explicit type.
// Nodes with "properties" default to "object"; other nodes default to "object" as well.
// $ref/anyOf/oneOf/allOf nodes are left as leaves — see isCompositionNode.
func ensureTypeFields(schema shared.FunctionParameters) shared.FunctionParameters {
	if schema == nil {
		return nil
	}

	walkSchema(schema, func(node map[string]any) {
		if isCompositionNode(node) {
			return
		}
		if _, hasType := node["type"]; !hasType {
			node["type"] = "object"
		}
	})

	return schema
}

// removeFormatFields removes the "format" field from all nodes in the schema.
// OpenAI does not support the JSON Schema "format" keyword (e.g. "uri", "email", "date").
func removeFormatFields(schema shared.FunctionParameters) shared.FunctionParameters {
	if schema == nil {
		return nil
	}

	walkSchema(schema, func(node map[string]any) {
		delete(node, "format")
	})

	return schema
}

// In Docker Desktop 4.52, the MCP Gateway produces an invalid tools shema for `mcp-config-set`.
func fixSchemaArrayItems(schema shared.FunctionParameters) shared.FunctionParameters {
	propertiesValue, ok := schema["properties"]
	if !ok {
		return schema
	}

	properties, ok := propertiesValue.(map[string]any)
	if !ok {
		return schema
	}

	for _, propValue := range properties {
		prop, ok := propValue.(map[string]any)
		if !ok {
			continue
		}

		checkForMissingItems := false
		switch t := prop["type"].(type) {
		case string:
			checkForMissingItems = t == "array"
		case []string:
			checkForMissingItems = slices.Contains(t, "array")
		}
		if !checkForMissingItems {
			continue
		}

		if _, ok := prop["items"]; !ok {
			prop["items"] = map[string]any{"type": "object"}
		}
	}

	return schema
}
