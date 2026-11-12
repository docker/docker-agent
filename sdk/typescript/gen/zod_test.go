package main

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestZodOf(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		schema string
		want   string
	}{
		"string":           {`{"type":"string","description":"ignored"}`, "z.string()"},
		"string bounds":    {`{"type":"string","minLength":1,"maxLength":3}`, "z.string().min(1).max(3)"},
		"integer bounds":   {`{"type":"integer","minimum":1,"maximum":300}`, "z.int().min(1).max(300)"},
		"exclusive bounds": {`{"type":"number","exclusiveMinimum":0,"exclusiveMaximum":1}`, "z.number().gt(0).lt(1)"},
		"nullable":         {`{"type":["null","integer"]}`, "z.int().nullable()"},
		"union of types":   {`{"type":["string","boolean"]}`, "z.union([z.string(), z.boolean()])"},
		"string enum":      {`{"type":"string","enum":["a","b"]}`, "z.enum(['a', 'b'])"},
		"mixed enum":       {`{"enum":["a",1]}`, "z.union([z.literal('a'), z.literal(1)])"},
		"const":            {`{"const":"x"}`, "z.literal('x')"},
		"typed const":      {`{"const":1,"type":"integer","description":"ignored"}`, "z.literal(1)"},
		"array":            {`{"type":"array","items":{"type":"string"},"minItems":1}`, "z.array(z.string()).min(1)"},
		"any":              {`{}`, "z.unknown()"},
		"map of any":       {`{"type":"object","additionalProperties":true}`, "z.record(z.string(), z.unknown())"},
		"typed map":        {`{"type":"object","additionalProperties":{"type":"integer"}}`, "z.record(z.string(), z.int())"},
		"empty strict":     {`{"type":"object","additionalProperties":false}`, "z.strictObject({})"},
		"anyOf":            {`{"anyOf":[{"type":"string"},{"type":"null"}]}`, "z.union([z.string(), z.null()])"},
		"strict object": {
			`{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"boolean"},"$k":{"type":"string"}},"required":["b"],"additionalProperties":false}`,
			"z.strictObject({\n  b: z.string(),\n  a: z.boolean().optional(),\n  $k: z.string().optional(),\n})",
		},
		"open object": {
			`{"type":"object","properties":{"a-b":{"type":"object","properties":{"c":{"type":"string"}},"required":["c"]}}}`,
			"z.looseObject({\n  'a-b': z.looseObject({\n    c: z.string(),\n  }).optional(),\n})",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := zodOf(decodeSchema(t, tt.schema), "")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestZodOfFailsOnUnsupportedSchemas(t *testing.T) {
	t.Parallel()

	for _, schema := range []string{
		`{"$ref":"#/$defs/X"}`,
		`{"allOf":[{"type":"string"}]}`,
		`{"not":{"type":"string"}}`,
		`{"type":"string","pattern":"^a$"}`,
		`{"type":"string","format":"date-time"}`,
		`{"type":"array","uniqueItems":true}`,
		`{"type":"object","patternProperties":{"^a":{}}}`,
		`{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"string"}}`,
		`{"type":"object","properties":{"a":{"type":"string"}},"required":["b"]}`,
		`{"type":"object","properties":{"a":{"type":"string","pattern":"x"}}}`,
		`{"type":"object","required":["a"]}`,
		`{"type":"object","required":["a"],"additionalProperties":false}`,
		`{"const":"x","minLength":1}`,
		`{"const":"x","type":"integer"}`,
		`{"enum":["a"],"type":"integer"}`,
		`{"enum":["a"],"maxLength":1}`,
		`{"anyOf":[{"type":"string"}],"type":"string"}`,
		`{"oneOf":[{"type":"string"}],"minLength":1}`,
	} {
		_, err := zodOf(decodeSchema(t, schema), "")
		assert.ErrorIsf(t, err, errUnsupportedZod, "%s", schema)
	}
}

func TestUniqueToolArgsRejectsConflictingSchemas(t *testing.T) {
	t.Parallel()

	toolsets := []toolsetInfo{
		{name: "a", tools: []toolInfo{{name: "read", params: decodeSchema(t, `{"type":"string"}`)}}},
		{name: "b", tools: []toolInfo{{name: "read", params: decodeSchema(t, `{"type":"integer"}`)}}},
	}
	_, err := uniqueToolArgs(toolsets)
	require.ErrorContains(t, err, "takes different arguments")

	toolsets[1].tools[0].params = decodeSchema(t, `{"type":"string"}`)
	args, err := uniqueToolArgs(toolsets)
	require.NoError(t, err)
	assert.Len(t, args, 1)
}

func TestEmitZodRejectsNameCollisions(t *testing.T) {
	t.Parallel()

	toolsets := []toolsetInfo{{name: "shell", tools: []toolInfo{{name: "shell", params: decodeSchema(t, `{}`)}}}}
	args, err := uniqueToolArgs(toolsets)
	require.NoError(t, err)

	_, err = emitZod(toolsets, args, map[string]bool{"ShellArgs": true})
	require.ErrorContains(t, err, "ShellArgs is exported twice")

	_, err = emitZod([]toolsetInfo{{name: "console", tools: toolsets[0].tools}}, args, nil)
	require.ErrorContains(t, err, "console shadows a global")

	files, err := emitZod(toolsets, args, nil)
	require.NoError(t, err)
	assert.Contains(t, files[toolArgsFile], "export const ShellArgsSchema = z.unknown();")
	assert.Contains(t, files[toolsetsFile], "export const shell = ['shell'] as const;")
}

func TestNames(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "SearchRemoteMCPServers", pascalName("search_remote_mcp_servers"))
	assert.Equal(t, "LSPWorkspaceSymbols", pascalName("lsp_workspace_symbols"))
	assert.Equal(t, "OpenURL", pascalName("open_url"))
	assert.Equal(t, "openURL", camelName("open_url"))
	assert.Equal(t, "mcpCatalog", camelName("mcp_catalog"))
	assert.Equal(t, "shell", camelName("shell"))
}

func decodeSchema(t *testing.T, data string) *jsonschema.Schema {
	t.Helper()
	var s jsonschema.Schema
	require.NoError(t, json.Unmarshal([]byte(data), &s))
	require.NoError(t, applyPropertyOrder(&s, []byte(data)))
	return &s
}
