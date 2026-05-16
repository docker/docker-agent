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

	return fixSchemaArrayItems(removeFormatFields(ensureTypeFields(makeAllRequired(p)))), nil
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
// Also ensures all object-type schemas have additionalProperties: false.
func makeAllRequired(schema shared.FunctionParameters) shared.FunctionParameters {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}

	walkSchema(schema, func(node map[string]any) {
		// Check if this node is an object type (either "object" or ["object", ...])
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

		// All object types must have additionalProperties: false for OpenAI Responses API strict mode
		// But only set it if additionalProperties is not already defined as an object schema
		if isObject {
			if addProps, exists := node["additionalProperties"]; !exists || addProps == nil || addProps == true {
				node["additionalProperties"] = false
			}
			// If additionalProperties is already set to false or is an object schema (map[string]any),
			// leave it as is - the object schema case will be walked separately
		}

		// If the node has explicit properties, make them all required
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
	})

	return schema
}

// ensureTypeFields ensures every schema node that is a map has a "type" key.
// OpenAI Responses API requires all schema nodes to have an explicit type.
// Nodes with "properties" default to "object"; other nodes default to "object" as well.
func ensureTypeFields(schema shared.FunctionParameters) shared.FunctionParameters {
	if schema == nil {
		return nil
	}

	walkSchema(schema, func(node map[string]any) {
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
