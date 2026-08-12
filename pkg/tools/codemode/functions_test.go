package codemode

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestToolToTypeScript(t *testing.T) {
	t.Parallel()
	type CreateTodoArgs struct {
		Description string   `json:"description" jsonschema:"Description of the todo item"`
		Labels      []string `json:"labels,omitempty" jsonschema:"Labels to apply"`
	}

	tool := tools.Tool{
		Name:         "create_todo",
		Description:  "Create new todo\n each of them with a description",
		Parameters:   tools.MustSchemaFor[CreateTodoArgs](),
		OutputSchema: tools.MustSchemaFor[string](),
	}

	declaration := toolToTypeScript(tool)

	assert.Equal(t, `/**
 * Create new todo
 * each of them with a description
 */
interface CreateTodoInput {
  // Description of the todo item
  description: string;
  // Labels to apply
  labels?: null | string[];
}

type CreateTodoOutput = string;

declare function CreateTodo(args: CreateTodoInput): CreateTodoOutput;
`, declaration)
}

func TestToolToTypeScriptExamples(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		parameters map[string]any
		output     map[string]any
		want       string
	}{
		{
			name:       "primitive aliases",
			parameters: map[string]any{"type": "string"},
			output:     map[string]any{"type": "boolean"},
			want: `/**
 * Example tool
 */
type ExampleToolInput = string;

type ExampleToolOutput = boolean;

declare function ExampleTool(args: ExampleToolInput): ExampleToolOutput;
`,
		},
		{
			name: "nested object, enum, nullable array, and property comments",
			parameters: map[string]any{
				"type":     "object",
				"required": []string{"filter"},
				"properties": map[string]any{
					"filter": map[string]any{
						"type":        "object",
						"description": "Filter to apply\n before searching",
						"properties": map[string]any{
							"sort-order": map[string]any{"enum": []any{"asc", "desc"}},
						},
					},
				},
				"additionalProperties": false,
			},
			output: map[string]any{
				"type":  []any{"array", "null"},
				"items": map[string]any{"type": "number"},
			},
			want: `/**
 * Example tool
 */
interface ExampleToolInput {
  // Filter to apply
  // before searching
  filter: {
    "sort-order"?: "asc" | "desc";
  };
}

type ExampleToolOutput = number[] | null;

declare function ExampleTool(args: ExampleToolInput): ExampleToolOutput;
`,
		},
		{
			name: "unions and intersections",
			parameters: map[string]any{
				"oneOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "number"},
				},
			},
			output: map[string]any{
				"allOf": []any{
					map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
					map[string]any{"type": "object", "properties": map[string]any{"active": map[string]any{"type": "boolean"}}},
				},
			},
			want: `/**
 * Example tool
 */
type ExampleToolInput = string | number;

type ExampleToolOutput = {
  id?: string;
} & {
  active?: boolean;
};

declare function ExampleTool(args: ExampleToolInput): ExampleToolOutput;
`,
		},
		{
			name: "references and typed additional properties",
			parameters: map[string]any{
				"type": "object",
				"$defs": map[string]any{
					"identifier": map[string]any{"type": "integer"},
				},
				"properties": map[string]any{
					"id": map[string]any{"$ref": "#/$defs/identifier"},
				},
				"additionalProperties": map[string]any{"type": "string"},
			},
			output: map[string]any{"const": "ok"},
			want: `/**
 * Example tool
 */
interface ExampleToolInput {
  id?: number;
  [key: string]: string;
}

type ExampleToolOutput = "ok";

declare function ExampleTool(args: ExampleToolInput): ExampleToolOutput;
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tool := tools.Tool{
				Name:         "example_tool",
				Description:  "Example tool",
				Parameters:   tt.parameters,
				OutputSchema: tt.output,
			}
			assert.Equal(t, tt.want, toolToTypeScript(tool))
		})
	}
}

func TestSchemaType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		schema map[string]any
		want   string
	}{
		{name: "string", schema: map[string]any{"type": "string"}, want: "string"},
		{name: "integer", schema: map[string]any{"type": "integer"}, want: "number"},
		{name: "number", schema: map[string]any{"type": "number"}, want: "number"},
		{name: "boolean", schema: map[string]any{"type": "boolean"}, want: "boolean"},
		{name: "null", schema: map[string]any{"type": "null"}, want: "null"},
		{name: "const", schema: map[string]any{"const": "fixed"}, want: `"fixed"`},
		{name: "mixed enum", schema: map[string]any{"enum": []any{"ready", 2.0, true, nil}}, want: `"ready" | 2 | true | null`},
		{name: "oneOf", schema: map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}}}, want: "string | number"},
		{name: "anyOf", schema: map[string]any{"anyOf": []any{map[string]any{"type": "boolean"}, map[string]any{"type": "null"}}}, want: "boolean | null"},
		{name: "allOf", schema: map[string]any{"allOf": []any{map[string]any{"type": "string"}, map[string]any{"const": "x"}}}, want: `string & "x"`},
		{name: "nullable", schema: map[string]any{"type": []any{"string", "null"}}, want: "string | null"},
		{name: "array", schema: map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, want: "number[]"},
		{name: "array of union", schema: map[string]any{"type": "array", "items": map[string]any{"type": []any{"string", "null"}}}, want: "(string | null)[]"},
		{name: "inferred array", schema: map[string]any{"items": map[string]any{"type": "boolean"}}, want: "boolean[]"},
		{name: "unknown", schema: map[string]any{}, want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, schemaType(tt.schema, tt.schema, 0))
		})
	}
}

func TestSchemaTypeReferences(t *testing.T) {
	t.Parallel()

	root := map[string]any{
		"$defs": map[string]any{
			"plain": map[string]any{"type": "string"},
			"a/b~c": map[string]any{"type": "boolean"},
		},
	}

	assert.Equal(t, "string", schemaType(map[string]any{"$ref": "#/$defs/plain"}, root, 0))
	assert.Equal(t, "boolean", schemaType(map[string]any{"$ref": "#/$defs/a~1b~0c"}, root, 0))
	assert.Equal(t, "unknown", schemaType(map[string]any{"$ref": "https://example.com/schema"}, root, 0))
	assert.Equal(t, "unknown", schemaType(map[string]any{"$ref": "#/$defs/missing"}, root, 0))
}

func TestObjectType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		additional any
		want       string
	}{
		{name: "forbidden", additional: false, want: "{\n  id: string;\n}"},
		{name: "allowed", additional: true, want: "{\n  id: string;\n  [key: string]: unknown;\n}"},
		{name: "typed", additional: map[string]any{"type": "number"}, want: "{\n  id: string;\n  [key: string]: number;\n}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			schema := map[string]any{
				"type":                 "object",
				"required":             []any{"id"},
				"properties":           map[string]any{"id": map[string]any{"type": "string"}},
				"additionalProperties": tt.additional,
			}
			assert.Equal(t, tt.want, objectType(schema, schema, 0))
		})
	}
}

func TestObjectTypePropertyNamesAndComments(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"validName": map[string]any{
				"type":        "string",
				"description": "First line\n Second line",
			},
			"with-dash": map[string]any{"type": "boolean"},
		},
	}

	assert.Equal(t, `{
  // First line
  // Second line
  validName?: string;
  "with-dash"?: boolean;
}`, objectType(schema, schema, 0))
}

func TestSchemaMapAndLiteralFallbacks(t *testing.T) {
	t.Parallel()

	assert.Nil(t, schemaMap(make(chan int)))
	assert.Equal(t, "unknown", literal(math.Inf(1)))
}

func TestTypeName(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"create_todo":  "CreateTodo",
		"search-items": "SearchItems",
		"2fa":          "Tool2fa",
		"---":          "Tool",
		"über_tool":    "ÜberTool",
	}
	for input, want := range tests {
		assert.Equal(t, want, typeName(input))
	}
}

func TestToolToTypeScriptNestedAndNullableTypes(t *testing.T) {
	t.Parallel()

	tool := tools.Tool{
		Name:        "search-items",
		Description: "Search items",
		Parameters: map[string]any{
			"type":     "object",
			"required": []string{"filter"},
			"properties": map[string]any{
				"filter": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"sort-order": map[string]any{"enum": []any{"asc", "desc"}},
					},
				},
			},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{
			"type":  []any{"array", "null"},
			"items": map[string]any{"type": "number"},
		},
	}

	declaration := toolToTypeScript(tool)

	assert.Contains(t, declaration, `interface SearchItemsInput {
  filter: {
    "sort-order"?: "asc" | "desc";
  };
}`)
	assert.Contains(t, declaration, "type SearchItemsOutput = number[] | null;")
	assert.Contains(t, declaration, "declare function SearchItems(args: SearchItemsInput): SearchItemsOutput;")
}
