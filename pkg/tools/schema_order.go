package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
)

const schemaPropertyOrder = "x-docker-agent-property-order"

// schemaWithOrder keeps declaration order out of the public JSON Schema keywords.
type schemaWithOrder map[string]any

func (s schemaWithOrder) MarshalJSON() ([]byte, error) {
	return json.Marshal(orderedSchemaValue(map[string]any(s)))
}

// SchemaToOrderedMap decodes a schema without normalizing its shape.
func SchemaToOrderedMap(schema any) (map[string]any, error) {
	if ordered, ok := schema.(schemaWithOrder); ok {
		schema = map[string]any(ordered)
	}
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return decodeSchema(data)
}

// SchemaPropertyNames returns properties in declaration order, with newly added
// properties appended deterministically.
func SchemaPropertyNames(schema map[string]any) []string {
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	var names []string
	switch order := schema[schemaPropertyOrder].(type) {
	case []string:
		names = slices.Clone(order)
	case []any:
		for _, name := range order {
			if name, ok := name.(string); ok {
				names = append(names, name)
			}
		}
	}
	result := make([]string, 0, len(properties))
	for _, name := range append(names, slices.Sorted(maps.Keys(properties))...) {
		if _, exists := properties[name]; exists && !slices.Contains(result, name) {
			result = append(result, name)
		}
	}
	return result
}

// OrderedSchemaProperties prepares a normalized schema for wire serialization.
func OrderedSchemaProperties(schema map[string]any) {
	ordered := orderedSchemaValue(schema).(map[string]any)
	clear(schema)
	maps.Copy(schema, ordered)
}

func orderedSchemaValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, child := range value {
			if key != schemaPropertyOrder {
				result[key] = orderedSchemaValue(child)
			}
		}
		if properties, ok := result["properties"].(map[string]any); ok {
			result["properties"] = orderedProperties{properties: properties, order: SchemaPropertyNames(value)}
		}
		return result
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			result[i] = orderedSchemaValue(child)
		}
		return result
	default:
		return value
	}
}

func recordDeclarationOrder(schema map[string]any, typ reflect.Type) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			return
		}
		var order []string
		for _, field := range reflect.VisibleFields(typ) {
			if field.Anonymous || !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" {
				name = field.Name
			}
			if child, ok := properties[name].(map[string]any); ok {
				order = append(order, name)
				recordDeclarationOrder(child, field.Type)
			}
		}
		schema[schemaPropertyOrder] = order
	case reflect.Slice, reflect.Array:
		if items, ok := schema["items"].(map[string]any); ok {
			recordDeclarationOrder(items, typ.Elem())
		}
	case reflect.Map:
		if child, ok := schema["additionalProperties"].(map[string]any); ok {
			recordDeclarationOrder(child, typ.Elem())
		}
	}
}

type orderedProperties struct {
	properties map[string]any
	order      []string
}

func (p orderedProperties) MarshalJSON() ([]byte, error) {
	var out bytes.Buffer
	out.WriteByte('{')
	for i, name := range p.order {
		key, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(p.properties[name])
		if err != nil {
			return nil, err
		}
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(key)
		out.WriteByte(':')
		out.Write(data)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// decodeSchema preserves property order across JSON round trips, including remote tools.
func decodeSchema(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	value, _, err := decodeOrderedValue(decoder)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("tool schema must be a JSON object")
	}
	return object, nil
}

func decodeOrderedValue(decoder *json.Decoder) (any, []string, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, nil, err
	}
	switch token {
	case json.Delim('{'):
		object := make(map[string]any)
		var keys, propertyOrder []string
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, nil, err
			}
			value, order, err := decodeOrderedValue(decoder)
			if err != nil {
				return nil, nil, err
			}
			name := key.(string)
			keys = append(keys, name)
			object[name] = value
			if name == "properties" {
				propertyOrder = order
			}
		}
		if _, err := decoder.Token(); err != nil {
			return nil, nil, err
		}
		if _, exists := object[schemaPropertyOrder]; !exists && propertyOrder != nil {
			object[schemaPropertyOrder] = propertyOrder
		}
		return object, keys, nil
	case json.Delim('['):
		var values []any
		for decoder.More() {
			value, _, err := decodeOrderedValue(decoder)
			if err != nil {
				return nil, nil, err
			}
			values = append(values, value)
		}
		_, err := decoder.Token()
		return values, nil, err
	default:
		return token, nil, nil
	}
}
