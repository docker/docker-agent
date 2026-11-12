package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	toolArgsFile = "toolArgs.ts"
	toolsetsFile = "toolsets.ts"
)

// toolArgs is the argument schema of one tool, shared by every toolset that
// offers a tool of that name.
type toolArgs struct {
	tool   string
	name   string
	schema *jsonschema.Schema
}

// uniqueToolArgs dedupes tools offered by several toolsets, which must then
// take the same arguments.
func uniqueToolArgs(toolsets []toolsetInfo) ([]toolArgs, error) {
	seen := map[string]toolArgs{}
	for _, ts := range toolsets {
		for _, tool := range ts.tools {
			args := toolArgs{tool: tool.name, name: pascalName(tool.name) + "Args", schema: tool.params}
			if prev, ok := seen[tool.name]; ok {
				same, err := sameSchema(prev.schema, args.schema)
				if err != nil {
					return nil, err
				}
				if !same {
					return nil, fmt.Errorf("tool %s takes different arguments in different toolsets", tool.name)
				}
				continue
			}
			seen[tool.name] = args
		}
	}
	var out []toolArgs
	for _, name := range sortedKeys(seen) {
		out = append(out, seen[name])
	}
	return out, nil
}

// emitZod renders the tool argument schemas and the per-toolset tool names.
// taken holds the names other generated files already export.
func emitZod(toolsets []toolsetInfo, args []toolArgs, taken map[string]bool) (map[string]string, error) {
	exported := map[string]bool{}
	claim := func(name string) error {
		if reservedNames[name] && !allowedShadows[name] {
			return fmt.Errorf("%s shadows a global", name)
		}
		if taken[name] || exported[name] {
			return fmt.Errorf("%s is exported twice", name)
		}
		exported[name] = true
		return nil
	}

	var schemas strings.Builder
	schemas.WriteString(header + "\nimport { z } from 'zod';\n\nimport type { ToolName } from './toolsets.ts';\n")
	for _, a := range args {
		if err := claim(a.name + "Schema"); err != nil {
			return nil, err
		}
		if err := claim(a.name); err != nil {
			return nil, err
		}
		expr, err := zodOf(a.schema, "")
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", a.tool, err)
		}
		fmt.Fprintf(&schemas, "\n/** Arguments of the `%s` tool. */\nexport const %sSchema = %s;\nexport type %s = z.infer<typeof %sSchema>;\n", a.tool, a.name, expr, a.name, a.name)
	}
	schemas.WriteString("\n/** Argument schemas by tool name. */\nexport const toolArgsSchemas = {\n")
	for _, a := range args {
		fmt.Fprintf(&schemas, "  %s: %sSchema,\n", propertyKey(a.tool), a.name)
	}
	schemas.WriteString("} as const satisfies Record<ToolName, z.ZodType>;\n")
	if err := claim("toolArgsSchemas"); err != nil {
		return nil, err
	}

	var names strings.Builder
	names.WriteString(header)
	sorted := slices.Clone(toolsets)
	slices.SortFunc(sorted, func(a, b toolsetInfo) int { return strings.Compare(a.name, b.name) })
	var members []string
	for _, ts := range sorted {
		if err := claim(ts.name); err != nil {
			return nil, err
		}
		var quoted []string
		for _, tool := range ts.tools {
			l, err := literal(tool.name)
			if err != nil {
				return nil, err
			}
			quoted = append(quoted, l)
		}
		if line := fmt.Sprintf("export const %s = [%s] as const;", ts.name, strings.Join(quoted, ", ")); len(line) <= 100 {
			fmt.Fprintf(&names, "\n%s\n", line)
		} else {
			fmt.Fprintf(&names, "\nexport const %s = [\n  %s,\n] as const;\n", ts.name, strings.Join(quoted, ",\n  "))
		}
		members = append(members, fmt.Sprintf("(typeof %s)[number]", ts.name))
	}
	if err := claim("ToolName"); err != nil {
		return nil, err
	}
	fmt.Fprintf(&names, "\n/** Name of a tool offered by a builtin toolset. */\nexport type ToolName =\n  | %s;\n", strings.Join(members, "\n  | "))

	return map[string]string{toolArgsFile: schemas.String(), toolsetsFile: names.String()}, nil
}

// emitToolArgsJSONSchema ships the argument schemas as $defs.
func emitToolArgsJSONSchema(args []toolArgs) (string, error) {
	doc := &jsonschema.Schema{Schema: "https://json-schema.org/draft/2020-12/schema", Defs: map[string]*jsonschema.Schema{}}
	for _, a := range args {
		s := a.schema.CloneSchemas()
		s.Comment = a.tool
		doc.Defs[a.name] = s
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

var errUnsupportedZod = errors.New("unsupported by the Zod emitter")

// zodKeywords are the JSON Schema keywords zodOf understands, besides the
// annotations it ignores.
var zodKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true, "items": true,
	"enum": true, "const": true, "anyOf": true, "oneOf": true,
	"minimum": true, "maximum": true, "exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true,
	"minLength": true, "maxLength": true, "minItems": true, "maxItems": true,
}

// annotations don't affect validation.
var annotations = map[string]bool{
	"title": true, "description": true, "$comment": true, "examples": true, "default": true, "deprecated": true,
}

// zodOf renders s as a Zod v4 expression. Objects are strict unless the
// schema allows additional properties.
func zodOf(s *jsonschema.Schema, indent string) (string, error) {
	if err := checkZodKeywords(s); err != nil {
		return "", err
	}
	switch {
	case s.Const != nil:
		if err := onlyKeywords(s, "const", "type"); err != nil {
			return "", err
		}
		if err := matchesType(s, *s.Const); err != nil {
			return "", err
		}
		l, err := literal(*s.Const)
		if err != nil {
			return "", err
		}
		return "z.literal(" + l + ")", nil
	case len(s.Enum) > 0:
		if err := onlyKeywords(s, "enum", "type"); err != nil {
			return "", err
		}
		if err := matchesType(s, s.Enum...); err != nil {
			return "", err
		}
		return zodEnum(s)
	case len(s.AnyOf) > 0 || len(s.OneOf) > 0:
		if err := onlyKeywords(s, "anyOf", "oneOf"); err != nil {
			return "", err
		}
		var members []string
		for _, m := range slices.Concat(s.AnyOf, s.OneOf) {
			expr, err := zodOf(m, indent)
			if err != nil {
				return "", err
			}
			members = append(members, expr)
		}
		if len(members) == 1 {
			return members[0], nil
		}
		return "z.union([" + strings.Join(members, ", ") + "])", nil
	case len(s.Types) > 0:
		var types []string
		nullable := false
		for _, t := range s.Types {
			if t == "null" {
				nullable = true
			} else {
				types = append(types, t)
			}
		}
		var members []string
		for _, t := range types {
			single := *s
			single.Types, single.Type = nil, t
			expr, err := zodOf(&single, indent)
			if err != nil {
				return "", err
			}
			members = append(members, expr)
		}
		var expr string
		switch len(members) {
		case 0:
			expr = "z.null()"
			nullable = false
		case 1:
			expr = members[0]
		default:
			expr = "z.union([" + strings.Join(members, ", ") + "])"
		}
		if nullable {
			expr += ".nullable()"
		}
		return expr, nil
	}

	switch s.Type {
	case "string":
		return "z.string()" + bounds(s.MinLength, s.MaxLength), nil
	case "integer":
		return "z.int()" + numberBounds(s), nil
	case "number":
		return "z.number()" + numberBounds(s), nil
	case "boolean":
		return "z.boolean()", nil
	case "null":
		return "z.null()", nil
	case "array":
		items := "z.unknown()"
		if s.Items != nil {
			expr, err := zodOf(s.Items, indent)
			if err != nil {
				return "", err
			}
			items = expr
		}
		return "z.array(" + items + ")" + bounds(s.MinItems, s.MaxItems), nil
	case "object":
		return zodObject(s, indent)
	case "":
		if isEmptySchema(s) {
			return "z.unknown()", nil
		}
	}
	return "", fmt.Errorf("%w: type %q", errUnsupportedZod, s.Type)
}

func zodObject(s *jsonschema.Schema, indent string) (string, error) {
	for _, name := range s.Required {
		if _, ok := s.Properties[name]; !ok {
			return "", fmt.Errorf("%w: required property %q is not declared", errUnsupportedZod, name)
		}
	}
	extra := s.AdditionalProperties
	if len(s.Properties) == 0 {
		switch {
		case extra != nil && isFalse(extra):
			return "z.strictObject({})", nil
		case extra == nil || isEmptySchema(extra):
			return "z.record(z.string(), z.unknown())", nil
		default:
			values, err := zodOf(extra, indent)
			if err != nil {
				return "", err
			}
			return "z.record(z.string(), " + values + ")", nil
		}
	}

	constructor := "z.looseObject"
	switch {
	case extra != nil && isFalse(extra):
		constructor = "z.strictObject"
	case extra != nil && !isEmptySchema(extra):
		return "", fmt.Errorf("%w: properties with typed additionalProperties", errUnsupportedZod)
	}

	inner := indent + "  "
	var b strings.Builder
	b.WriteString(constructor + "({\n")
	for _, name := range propertyNames(s) {
		expr, err := zodOf(s.Properties[name], inner)
		if err != nil {
			return "", fmt.Errorf("property %s: %w", name, err)
		}
		if !slices.Contains(s.Required, name) {
			expr += ".optional()"
		}
		fmt.Fprintf(&b, "%s%s: %s,\n", inner, propertyKey(name), expr)
	}
	b.WriteString(indent + "})")
	return b.String(), nil
}

func zodEnum(s *jsonschema.Schema) (string, error) {
	var literals []string
	allStrings := true
	for _, v := range s.Enum {
		if _, ok := v.(string); !ok {
			allStrings = false
		}
		l, err := literal(v)
		if err != nil {
			return "", err
		}
		literals = append(literals, l)
	}
	if allStrings {
		return "z.enum([" + strings.Join(literals, ", ") + "])", nil
	}
	for i, l := range literals {
		literals[i] = "z.literal(" + l + ")"
	}
	return "z.union([" + strings.Join(literals, ", ") + "])", nil
}

func numberBounds(s *jsonschema.Schema) string {
	var b strings.Builder
	for _, bound := range []struct {
		method string
		value  *float64
	}{
		{"min", s.Minimum}, {"max", s.Maximum}, {"gt", s.ExclusiveMinimum}, {"lt", s.ExclusiveMaximum}, {"multipleOf", s.MultipleOf},
	} {
		if bound.value != nil {
			fmt.Fprintf(&b, ".%s(%s)", bound.method, strconv.FormatFloat(*bound.value, 'g', -1, 64))
		}
	}
	return b.String()
}

func bounds(minimum, maximum *int) string {
	var b strings.Builder
	if minimum != nil {
		fmt.Fprintf(&b, ".min(%d)", *minimum)
	}
	if maximum != nil {
		fmt.Fprintf(&b, ".max(%d)", *maximum)
	}
	return b.String()
}

// checkZodKeywords fails on keywords zodOf would otherwise silently drop.
func checkZodKeywords(s *jsonschema.Schema) error {
	return onlyKeywords(s, sortedKeys(zodKeywords)...)
}

// onlyKeywords fails when s has keywords besides annotations and allowed.
func onlyKeywords(s *jsonschema.Schema, allowed ...string) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if string(data) == "true" || string(data) == "false" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var unsupported []string
	for key := range fields {
		if !annotations[key] && !slices.Contains(allowed, key) {
			unsupported = append(unsupported, key)
		}
	}
	if len(unsupported) > 0 {
		slices.Sort(unsupported)
		return fmt.Errorf("%w: %s", errUnsupportedZod, strings.Join(unsupported, ", "))
	}
	return nil
}

// matchesType fails when s declares a type that one of values doesn't have.
func matchesType(s *jsonschema.Schema, values ...any) error {
	types := s.Types
	if s.Type != "" {
		types = []string{s.Type}
	}
	if len(types) == 0 {
		return nil
	}
	for _, v := range values {
		if !slices.ContainsFunc(types, func(t string) bool { return hasJSONType(v, t) }) {
			return fmt.Errorf("%w: %#v is not of type %v", errUnsupportedZod, v, types)
		}
	}
	return nil
}

func hasJSONType(v any, typ string) bool {
	switch v := v.(type) {
	case nil:
		return typ == "null"
	case string:
		return typ == "string"
	case bool:
		return typ == "boolean"
	case float64:
		return typ == "number" || (typ == "integer" && v == math.Trunc(v))
	case int, int64:
		return typ == "number" || typ == "integer"
	default:
		return false
	}
}

func isEmptySchema(s *jsonschema.Schema) bool {
	c := *s
	c.Title, c.Description, c.Comment, c.Examples, c.Default, c.Deprecated = "", "", "", nil, nil, false
	return reflect.ValueOf(c).IsZero()
}

func sameSchema(a, b *jsonschema.Schema) (bool, error) {
	da, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	db, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(da, db), nil
}
