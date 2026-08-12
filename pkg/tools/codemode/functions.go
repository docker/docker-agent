package codemode

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/docker/docker-agent/pkg/tools"
)

var typeScriptIdentifier = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func toolToTypeScript(tool tools.Tool) string {
	baseName := typeName(tool.Name)
	inputName := baseName + "Input"
	outputName := baseName + "Output"

	input := schemaMap(tool.Parameters)
	output := schemaMap(tool.OutputSchema)

	var doc strings.Builder
	writeDocComment(&doc, tool.Description)

	if isObjectSchema(input) {
		fmt.Fprintf(&doc, "interface %s %s\n\n", inputName, objectType(input, input, 0))
	} else {
		fmt.Fprintf(&doc, "type %s = %s;\n\n", inputName, schemaType(input, input, 0))
	}
	fmt.Fprintf(&doc, "type %s = %s;\n\n", outputName, schemaType(output, output, 0))
	fmt.Fprintf(&doc, "declare function %s(args: %s): %s;\n", baseName, inputName, outputName)

	return doc.String()
}

func schemaMap(schema any) map[string]any {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}
	return result
}

func schemaType(schema, root map[string]any, level int) string {
	if schema == nil {
		return "unknown"
	}
	if ref, ok := schema["$ref"].(string); ok {
		if resolved := resolveRef(root, ref); resolved != nil {
			return schemaType(resolved, root, level)
		}
	}
	if value, ok := schema["const"]; ok {
		return literal(value)
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		parts := make([]string, 0, len(values))
		for _, value := range values {
			parts = append(parts, literal(value))
		}
		return strings.Join(parts, " | ")
	}
	for _, keyword := range []struct {
		name      string
		separator string
	}{{"oneOf", " | "}, {"anyOf", " | "}, {"allOf", " & "}} {
		if variants, ok := schema[keyword.name].([]any); ok && len(variants) > 0 {
			parts := make([]string, 0, len(variants))
			for _, variant := range variants {
				if variantSchema, ok := variant.(map[string]any); ok {
					parts = append(parts, schemaType(variantSchema, root, level))
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, keyword.separator)
			}
		}
	}

	types := schemaTypes(schema)
	if len(types) > 1 {
		parts := make([]string, 0, len(types))
		for _, typ := range types {
			schemaCopy := cloneMap(schema)
			schemaCopy["type"] = typ
			parts = append(parts, schemaType(schemaCopy, root, level))
		}
		return strings.Join(parts, " | ")
	}

	typ := ""
	if len(types) == 1 {
		typ = types[0]
	}
	if typ == "" {
		switch {
		case schema["properties"] != nil || schema["additionalProperties"] != nil:
			typ = "object"
		case schema["items"] != nil:
			typ = "array"
		}
	}

	switch typ {
	case "object":
		return objectType(schema, root, level)
	case "array":
		itemSchema, _ := schema["items"].(map[string]any)
		itemType := schemaType(itemSchema, root, level)
		if strings.Contains(itemType, " | ") || strings.Contains(itemType, " & ") {
			itemType = "(" + itemType + ")"
		}
		return itemType + "[]"
	case "string":
		return "string"
	case "integer", "number":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	default:
		return "unknown"
	}
}

func objectType(schema, root map[string]any, level int) string {
	properties, _ := schema["properties"].(map[string]any)
	required := stringSet(schema["required"])
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	indent := strings.Repeat("  ", level)
	childIndent := strings.Repeat("  ", level+1)
	var result strings.Builder
	result.WriteString("{\n")
	for _, key := range keys {
		property, _ := properties[key].(map[string]any)
		if description, _ := property["description"].(string); description != "" {
			writeLineComments(&result, description, childIndent)
		}
		result.WriteString(childIndent)
		result.WriteString(propertyName(key))
		if !required[key] {
			result.WriteByte('?')
		}
		result.WriteString(": ")
		result.WriteString(schemaType(property, root, level+1))
		result.WriteString(";\n")
	}

	if additional, exists := schema["additionalProperties"]; exists {
		if allowed, ok := additional.(bool); ok && !allowed {
			result.WriteString(indent)
			result.WriteByte('}')
			return result.String()
		}
		additionalType := "unknown"
		if additionalSchema, ok := additional.(map[string]any); ok {
			additionalType = schemaType(additionalSchema, root, level+1)
		}
		result.WriteString(childIndent)
		fmt.Fprintf(&result, "[key: string]: %s;\n", additionalType)
	}
	result.WriteString(indent)
	result.WriteByte('}')
	return result.String()
}

func schemaTypes(schema map[string]any) []string {
	switch value := schema["type"].(type) {
	case string:
		return []string{value}
	case []any:
		result := make([]string, 0, len(value))
		for _, item := range value {
			if typ, ok := item.(string); ok {
				result = append(result, typ)
			}
		}
		return result
	default:
		return nil
	}
}

func isObjectSchema(schema map[string]any) bool {
	types := schemaTypes(schema)
	return len(types) == 1 && types[0] == "object"
}

func resolveRef(root map[string]any, ref string) map[string]any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var current any = root
	for part := range strings.SplitSeq(strings.TrimPrefix(ref, "#/"), "/") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
	}
	resolved, _ := current.(map[string]any)
	return resolved
}

func cloneMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	maps.Copy(result, value)
	return result
}

func stringSet(value any) map[string]bool {
	result := make(map[string]bool)
	if values, ok := value.([]any); ok {
		for _, item := range values {
			if text, ok := item.(string); ok {
				result[text] = true
			}
		}
	}
	return result
}

func typeName(name string) string {
	var result strings.Builder
	upperNext := true
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upperNext = true
			continue
		}
		if result.Len() == 0 && unicode.IsDigit(r) {
			result.WriteString("Tool")
		}
		if upperNext {
			r = unicode.ToUpper(r)
			upperNext = false
		}
		result.WriteRune(r)
	}
	if result.Len() == 0 {
		return "Tool"
	}
	return result.String()
}

func propertyName(name string) string {
	if typeScriptIdentifier.MatchString(name) {
		return name
	}
	data, _ := json.Marshal(name)
	return string(data)
}

func literal(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "unknown"
	}
	return string(data)
}

func writeLineComments(doc *strings.Builder, description, indent string) {
	for line := range strings.SplitSeq(description, "\n") {
		doc.WriteString(indent + "// " + strings.TrimSpace(line) + "\n")
	}
}

func writeDocComment(doc *strings.Builder, description string) {
	writeIndentedDocComment(doc, description, "")
}

func writeIndentedDocComment(doc *strings.Builder, description, indent string) {
	doc.WriteString(indent + "/**\n")
	for line := range strings.SplitSeq(description, "\n") {
		doc.WriteString(indent + " * " + strings.ReplaceAll(strings.TrimSpace(line), "*/", "*\\/") + "\n")
	}
	doc.WriteString(indent + " */\n")
}
