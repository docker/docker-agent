package openai

import (
	"maps"
	"slices"

	"github.com/openai/openai-go/v3/shared"

	"github.com/docker/docker-agent/pkg/tools"
)

// ConvertParametersToSchema converts parameters to OpenAI Schema format
func ConvertParametersToSchema(params any) (shared.FunctionParameters, error) {
	p, err := tools.SchemaToMap(params)
	if err != nil {
		return nil, err
	}

	return normalizeUnionTypes(fixSchemaArrayItems(removeFormatFields(makeAllRequired(p)))), nil
}

// walkSchema calls fn on the given schema node, then recursively walks into
// properties, anyOf/oneOf/allOf variants, array items, and additionalProperties.
func walkSchema(schema map[string]any, fn func(map[string]any)) {
	fn(schema)

	if properties, ok := schema["properties"].(map[string]any); ok {
		for _, v := range properties {
			if sub, ok := v.(map[string]any); ok {
				walkSchema(sub, fn)
			}
		}
	}

	for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
		if variants, ok := schema[keyword].([]any); ok {
			for _, v := range variants {
				if sub, ok := v.(map[string]any); ok {
					walkSchema(sub, fn)
				}
			}
		}
	}

	if items, ok := schema["items"].(map[string]any); ok {
		walkSchema(items, fn)
	}

	// additionalProperties can be a boolean or an object schema
	if additionalProps, ok := schema["additionalProperties"].(map[string]any); ok {
		walkSchema(additionalProps, fn)
	}
}

// makeAllRequired makes all object properties "required" throughout the schema,
// because that's what the OpenAI Response API demands.
// Properties that were not originally required are made nullable.
func makeAllRequired(schema shared.FunctionParameters) shared.FunctionParameters {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}

	walkSchema(schema, func(node map[string]any) {
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

			// Make newly-required properties nullable
			if !originallyRequired[propName] {
				if propMap, ok := properties[propName].(map[string]any); ok {
					if t, ok := propMap["type"].(string); ok {
						propMap["type"] = []string{t, "null"}
					}
				}
			}
		}

		node["required"] = newRequired
		node["additionalProperties"] = false
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

// normalizeUnionTypes converts union types like ["array", "null"] back to simple types
// for compatibility with AI gateways that don't support JSON Schema union types.
// This is needed for Cloudflare AI Gateway and similar proxies.
func normalizeUnionTypes(schema shared.FunctionParameters) shared.FunctionParameters {
	if schema == nil {
		return schema
	}

	// Convert union types at the current level
	// Only normalize nullable patterns: exactly 2 types where one is "null"
	if typeArray, ok := schema["type"].([]any); ok {
		if len(typeArray) == 2 {
			var hasNull bool
			var nonNullType string
			for _, t := range typeArray {
				if tStr, ok := t.(string); ok {
					if tStr == "null" {
						hasNull = true
					} else {
						nonNullType = tStr
					}
				}
			}
			if hasNull && nonNullType != "" {
				schema["type"] = nonNullType
			}
		}
	} else if typeArray, ok := schema["type"].([]string); ok {
		if len(typeArray) == 2 {
			var hasNull bool
			var nonNullType string
			for _, t := range typeArray {
				if t == "null" {
					hasNull = true
				} else {
					nonNullType = t
				}
			}
			if hasNull && nonNullType != "" {
				schema["type"] = nonNullType
			}
		}
	}

	// Convert nullable anyOf patterns like {"anyOf": [{"type":"string"},{"type":"null"}]} to {"type":"string"}
	// Only normalize when there are exactly 2 alternatives and one is {"type":"null"}.
	// This is needed for Gemini via Cloudflare which doesn't support anyOf in tool parameters.
	if anyOf, ok := schema["anyOf"].([]any); ok && len(anyOf) == 2 {
		hasNull := false
		var nonNullType string
		for _, item := range anyOf {
			if itemMap, ok := item.(map[string]any); ok {
				if typStr, ok := itemMap["type"].(string); ok {
					if typStr == "null" {
						hasNull = true
					} else {
						nonNullType = typStr
					}
				}
			}
		}
		if hasNull && nonNullType != "" {
			schema["type"] = nonNullType
			delete(schema, "anyOf")
		}
	}

	// Recursively handle properties
	if propertiesValue, ok := schema["properties"]; ok {
		if properties, ok := propertiesValue.(map[string]any); ok {
			for _, propValue := range properties {
				if prop, ok := propValue.(map[string]any); ok {
					normalizeUnionTypes(prop)
				}
			}
		}
	}

	// Recursively handle items (for arrays)
	if items, ok := schema["items"].(map[string]any); ok {
		normalizeUnionTypes(items)
	}

	// Recursively handle additionalProperties
	if addProps, ok := schema["additionalProperties"].(map[string]any); ok {
		normalizeUnionTypes(addProps)
	}

	return schema
}
