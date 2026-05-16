package dialog

import (
	"cmp"
	"encoding/json"
	"slices"
)

// parseElicitationSchema extracts fields from a JSON schema.
// Supports both object schemas with properties and primitive type schemas.
func parseElicitationSchema(schema any) []ElicitationField {
	schemaMap := toMap(schema)
	if schemaMap == nil {
		return nil
	}

	if properties, ok := schemaMap["properties"].(map[string]any); ok {
		return parseObjectSchema(schemaMap, properties)
	}
	return parsePrimitiveSchema(schemaMap)
}

func parseObjectSchema(schemaMap, properties map[string]any) []ElicitationField {
	requiredSet := toStringSet(schemaMap["required"])

	var fields []ElicitationField
	for name, propValue := range properties {
		if prop, ok := propValue.(map[string]any); ok {
			fields = append(fields, parseField(name, prop, requiredSet[name]))
		}
	}

	// Sort: required first, then alphabetically
	slices.SortFunc(fields, func(a, b ElicitationField) int {
		if a.Required != b.Required {
			if a.Required {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Name, b.Name)
	})

	return fields
}

func parsePrimitiveSchema(schemaMap map[string]any) []ElicitationField {
	schemaType, _ := schemaMap["type"].(string)
	if schemaType == "" || schemaType == "object" {
		return nil
	}

	name := "value"
	if title, _ := schemaMap["title"].(string); title != "" {
		name = title
	}

	return []ElicitationField{parseField(name, schemaMap, true)}
}

func parseField(name string, prop map[string]any, required bool) ElicitationField {
	field := ElicitationField{
		Name:     name,
		Type:     getString(prop, "type", "string"),
		Required: required,
	}

	field.Title, _ = prop["title"].(string)
	field.Description, _ = prop["description"].(string)
	field.Default = prop["default"]
	field.Format, _ = prop["format"].(string)
	field.Pattern, _ = prop["pattern"].(string)

	if enum, ok := prop["enum"].([]any); ok && len(enum) > 0 {
		field.Type = "enum"
		for _, e := range enum {
			if s, ok := e.(string); ok {
				field.EnumValues = append(field.EnumValues, s)
			}
		}
	}

	if v, ok := prop["minLength"].(float64); ok {
		field.MinLength = int(v)
	}
	if v, ok := prop["maxLength"].(float64); ok {
		field.MaxLength = int(v)
	}
	if v, ok := prop["minimum"].(float64); ok {
		field.Minimum = v
		field.HasMinimum = true
	}
	if v, ok := prop["maximum"].(float64); ok {
		field.Maximum = v
		field.HasMaximum = true
	}

	return field
}

// toMap converts various schema representations to map[string]any.
func toMap(schema any) map[string]any {
	switch s := schema.(type) {
	case map[string]any:
		return s
	case json.RawMessage:
		var m map[string]any
		if json.Unmarshal(s, &m) == nil {
			return m
		}
	default:
		if schema != nil {
			if data, err := json.Marshal(schema); err == nil {
				var m map[string]any
				if json.Unmarshal(data, &m) == nil {
					return m
				}
			}
		}
	}
	return nil
}

func toStringSet(v any) map[string]bool {
	arr, _ := v.([]any)
	set := make(map[string]bool, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			set[s] = true
		}
	}
	return set
}

func getString(m map[string]any, key, defaultVal string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return defaultVal
}
