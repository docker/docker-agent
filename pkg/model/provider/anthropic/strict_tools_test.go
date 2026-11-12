package anthropic

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func strictObject(props map[string]any, required ...string) map[string]any {
	m := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func strictTool(name string, schema map[string]any) tools.Tool {
	return tools.Tool{Name: name, Description: name, Parameters: schema}
}

// rootExtras returns the root keys ToolInputSchemaParam does not model
// (additionalProperties, $defs, ...). The SDK drops them on unmarshal, so
// the converter must carry them in ExtraFields for strict mode to work.
func rootExtras(t *testing.T, schema any) map[string]any {
	t.Helper()
	m, ok := marshalToMap(schema)
	require.True(t, ok)
	for _, k := range []string{"type", "properties", "required"} {
		delete(m, k)
	}
	return m
}

func makeTools(t *testing.T, reqTools ...tools.Tool) []anthropic.ToolUnionParam {
	t.Helper()
	params, err := convertTools(reqTools)
	require.NoError(t, err)
	for i, tool := range reqTools {
		params[i].OfTool.InputSchema.ExtraFields = rootExtras(t, tool.Parameters)
	}
	return params
}

func makeBetaTools(t *testing.T, reqTools ...tools.Tool) []anthropic.BetaToolUnionParam {
	t.Helper()
	params, err := convertBetaTools(reqTools)
	require.NoError(t, err)
	for i, tool := range reqTools {
		params[i].OfTool.InputSchema.ExtraFields = rootExtras(t, tool.Parameters)
	}
	return params
}

func toolStrictFlags(t *testing.T, params []anthropic.ToolUnionParam) map[string]bool {
	t.Helper()
	flags := map[string]bool{}
	for _, p := range params {
		require.NotNil(t, p.OfTool)
		flags[p.OfTool.Name] = p.OfTool.Strict.Valid() && p.OfTool.Strict.Value
	}
	return flags
}

func betaStrictFlags(t *testing.T, params []anthropic.BetaToolUnionParam) map[string]bool {
	t.Helper()
	flags := map[string]bool{}
	for _, p := range params {
		require.NotNil(t, p.OfTool)
		flags[p.OfTool.Name] = p.OfTool.Strict.Valid() && p.OfTool.Strict.Value
	}
	return flags
}

func TestStrictToolsOpt(t *testing.T) {
	t.Parallel()

	t.Run("absent", func(t *testing.T) {
		_, ok := strictToolsOpt(nil)
		assert.False(t, ok)
		_, ok = strictToolsOpt(map[string]any{"top_k": 40})
		assert.False(t, ok)
	})

	t.Run("bool", func(t *testing.T) {
		sel, ok := strictToolsOpt(map[string]any{"strict_tools": true})
		assert.True(t, ok)
		assert.True(t, sel.all)

		_, ok = strictToolsOpt(map[string]any{"strict_tools": false})
		assert.False(t, ok)
	})

	t.Run("names", func(t *testing.T) {
		sel, ok := strictToolsOpt(map[string]any{"strict_tools": []any{"search", "book"}})
		assert.True(t, ok)
		assert.False(t, sel.all)
		assert.Equal(t, []string{"search", "book"}, sel.names)
	})

	t.Run("invalid", func(t *testing.T) {
		_, ok := strictToolsOpt(map[string]any{"strict_tools": []any{}})
		assert.False(t, ok)
		_, ok = strictToolsOpt(map[string]any{"strict_tools": "search"})
		assert.False(t, ok)
		_, ok = strictToolsOpt(map[string]any{"strict_tools": []any{"search", 1}})
		assert.False(t, ok)
	})
}

func TestStrictTools_OptionalPropertiesStayOptional(t *testing.T) {
	t.Parallel()

	schema := strictObject(map[string]any{
		"city":  map[string]any{"type": "string"},
		"units": map[string]any{"type": "string", "enum": []any{"c", "f"}},
	}, "city")
	params := makeTools(t, strictTool("weather", schema))
	before, err := json.Marshal(params[0].OfTool.InputSchema)
	require.NoError(t, err)

	require.NoError(t, strictToolsSelection{all: true}.apply(params))

	assert.Equal(t, map[string]bool{"weather": true}, toolStrictFlags(t, params))
	after, err := json.Marshal(params[0].OfTool.InputSchema)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after), "input_schema must be untouched")
	assert.Equal(t, []string{"city"}, params[0].OfTool.InputSchema.Required)
}

func TestStrictTools_DoesNotMutateCallerSchema(t *testing.T) {
	t.Parallel()

	schema := strictObject(map[string]any{
		"filter": strictObject(map[string]any{"q": map[string]any{"type": "string"}}),
	})
	snapshot, err := json.Marshal(schema)
	require.NoError(t, err)

	params := makeTools(t, strictTool("search", schema))
	require.NoError(t, strictToolsSelection{names: []string{"search"}}.apply(params))

	current, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.JSONEq(t, string(snapshot), string(current))
	assert.True(t, params[0].OfTool.Strict.Value)
}

func TestStrictTools_Eligibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		schema  map[string]any
		wantErr string
	}{
		{
			name:   "flat object",
			schema: strictObject(map[string]any{"q": map[string]any{"type": "string"}}, "q"),
		},
		{
			name: "nullable and anyOf",
			schema: strictObject(map[string]any{
				"limit": map[string]any{"type": []any{"integer", "null"}},
				"id":    map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}},
			}),
		},
		{
			name: "local ref",
			schema: func() map[string]any {
				m := strictObject(map[string]any{"a": map[string]any{"$ref": "#/$defs/addr"}}, "a")
				m["$defs"] = map[string]any{"addr": strictObject(map[string]any{"zip": map[string]any{"type": "string"}}, "zip")}
				return m
			}(),
		},
		{
			name:   "supported format and minItems",
			schema: strictObject(map[string]any{"when": map[string]any{"type": "string", "format": "date-time"}, "tags": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}}),
		},
		{
			name: "root without additionalProperties false",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
			},
			wantErr: "#: object must set additionalProperties to false",
		},
		{
			name: "nested open object",
			schema: strictObject(map[string]any{
				"opts": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
			}),
			wantErr: "#/properties/opts: object must set additionalProperties to false",
		},
		{
			name:    "additionalProperties true",
			schema:  map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true},
			wantErr: "additionalProperties to false",
		},
		{
			name:    "additionalProperties schema",
			schema:  map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": map[string]any{"type": "string"}},
			wantErr: "additionalProperties to false",
		},
		{
			name: "nested unsupported keyword in array items",
			schema: strictObject(map[string]any{
				"ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 0}},
			}),
			wantErr: `#/properties/ids/items: keyword "minimum" is not supported`,
		},
		{
			name:    "oneOf",
			schema:  strictObject(map[string]any{"v": map[string]any{"oneOf": []any{map[string]any{"type": "string"}}}}),
			wantErr: `keyword "oneOf"`,
		},
		{
			name:    "unsupported format",
			schema:  strictObject(map[string]any{"v": map[string]any{"type": "string", "format": "binary"}}),
			wantErr: "format binary is not supported",
		},
		{
			name:    "minItems above one",
			schema:  strictObject(map[string]any{"v": map[string]any{"type": "array", "minItems": 2, "items": map[string]any{"type": "string"}}}),
			wantErr: "minItems must be 0 or 1",
		},
		{
			name:    "complex enum",
			schema:  strictObject(map[string]any{"v": map[string]any{"enum": []any{map[string]any{"a": 1}}}}),
			wantErr: "enum values must be",
		},
		{
			name:    "external ref",
			schema:  strictObject(map[string]any{"v": map[string]any{"$ref": "http://example.com/s.json"}}),
			wantErr: "only local $ref",
		},
		{
			name:    "dangling ref",
			schema:  strictObject(map[string]any{"v": map[string]any{"$ref": "#/$defs/missing"}}),
			wantErr: "does not resolve",
		},
		{
			name: "recursive ref",
			schema: func() map[string]any {
				m := strictObject(map[string]any{"n": map[string]any{"$ref": "#/$defs/node"}})
				m["$defs"] = map[string]any{"node": strictObject(map[string]any{"next": map[string]any{"$ref": "#/$defs/node"}})}
				return m
			}(),
			wantErr: "recursive $ref",
		},
		{
			name: "allOf with ref",
			schema: func() map[string]any {
				m := strictObject(map[string]any{"v": map[string]any{"allOf": []any{map[string]any{"$ref": "#/$defs/base"}}}})
				m["$defs"] = map[string]any{"base": strictObject(map[string]any{})}
				return m
			}(),
			wantErr: "allOf with $ref",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			params := makeTools(t, strictTool("t", tt.schema))

			err := strictToolsSelection{names: []string{"t"}}.apply(params)
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.True(t, params[0].OfTool.Strict.Value)
				return
			}
			require.ErrorContains(t, err, `strict_tools: tool "t": `)
			require.ErrorContains(t, err, tt.wantErr)
			assert.False(t, params[0].OfTool.Strict.Valid(), "ineligible tool must be left untouched")
		})
	}
}

func TestStrictTools_BlanketSkipsIneligible(t *testing.T) {
	t.Parallel()

	params := makeTools(t,
		strictTool("good", strictObject(map[string]any{"q": map[string]any{"type": "string"}}, "q")),
		strictTool("open", map[string]any{"type": "object", "properties": map[string]any{}}),
		strictTool("also_good", strictObject(map[string]any{})),
	)

	require.NoError(t, strictToolsSelection{all: true}.apply(params))
	assert.Equal(t, map[string]bool{"good": true, "open": false, "also_good": true}, toolStrictFlags(t, params))
}

func TestStrictTools_ExplicitNames(t *testing.T) {
	t.Parallel()

	good := strictObject(map[string]any{"q": map[string]any{"type": "string"}}, "q")
	open := map[string]any{"type": "object", "properties": map[string]any{}}

	t.Run("only named tools are marked", func(t *testing.T) {
		params := makeTools(t, strictTool("a", good), strictTool("b", good))
		require.NoError(t, strictToolsSelection{names: []string{"b"}}.apply(params))
		assert.Equal(t, map[string]bool{"a": false, "b": true}, toolStrictFlags(t, params))
	})

	t.Run("ineligible named tool fails fast", func(t *testing.T) {
		params := makeTools(t, strictTool("a", good), strictTool("b", open))
		err := strictToolsSelection{names: []string{"a", "b"}}.apply(params)
		require.ErrorContains(t, err, `strict_tools: tool "b"`)
		assert.True(t, params[0].OfTool.Strict.Value)
	})

	t.Run("missing named tool is tolerated", func(t *testing.T) {
		params := makeTools(t, strictTool("a", good))
		require.NoError(t, strictToolsSelection{names: []string{"a", "not_here"}}.apply(params))
		assert.True(t, params[0].OfTool.Strict.Value)
	})
}

func TestStrictTools_RequestLimits(t *testing.T) {
	t.Parallel()

	t.Run("strict tool count", func(t *testing.T) {
		var reqTools []tools.Tool
		for i := range maxStrictTools + 1 {
			reqTools = append(reqTools, strictTool(fmt.Sprintf("t%02d", i), strictObject(map[string]any{})))
		}
		params := makeTools(t, reqTools...)

		require.NoError(t, strictToolsSelection{all: true}.apply(params))
		flags := toolStrictFlags(t, params)
		assert.False(t, flags[fmt.Sprintf("t%02d", maxStrictTools)])
		delete(flags, fmt.Sprintf("t%02d", maxStrictTools))
		for name, strict := range flags {
			assert.True(t, strict, name)
		}
	})

	t.Run("optional parameters", func(t *testing.T) {
		props := map[string]any{}
		for i := range maxStrictOptional {
			props[fmt.Sprintf("p%02d", i)] = map[string]any{"type": "string"}
		}
		params := makeTools(t,
			strictTool("big", strictObject(props)),
			strictTool("one_more", strictObject(map[string]any{"x": map[string]any{"type": "string"}})),
			strictTool("required_only", strictObject(map[string]any{"x": map[string]any{"type": "string"}}, "x")),
		)

		require.NoError(t, strictToolsSelection{all: true}.apply(params))
		assert.Equal(t, map[string]bool{"big": true, "one_more": false, "required_only": true}, toolStrictFlags(t, params))

		err := strictToolsSelection{names: []string{"big", "one_more"}}.apply(params)
		require.ErrorContains(t, err, `tool "one_more"`)
		assert.ErrorContains(t, err, "optional parameters")
	})

	t.Run("union parameters counted at any depth", func(t *testing.T) {
		props := map[string]any{}
		for i := range maxStrictUnions {
			props[fmt.Sprintf("p%02d", i)] = map[string]any{"type": []any{"string", "null"}}
		}
		nested := strictObject(map[string]any{
			"inner": strictObject(map[string]any{"v": map[string]any{"anyOf": []any{map[string]any{"type": "string"}}}}, "v"),
		}, "inner")
		params := makeTools(t, strictTool("unions", strictObject(props)), strictTool("nested", nested))

		err := strictToolsSelection{names: []string{"unions", "nested"}}.apply(params)
		require.ErrorContains(t, err, `tool "nested"`)
		assert.ErrorContains(t, err, "union-typed parameters")
	})
}

func TestStrictTools_StandardBetaParity(t *testing.T) {
	t.Parallel()

	reqTools := []tools.Tool{
		strictTool("good", strictObject(map[string]any{"q": map[string]any{"type": "string"}}, "q")),
		strictTool("open", map[string]any{"type": "object", "properties": map[string]any{}}),
		strictTool("bad_kw", strictObject(map[string]any{"n": map[string]any{"type": "integer", "maximum": 10}})),
	}
	std := makeTools(t, reqTools...)
	beta := makeBetaTools(t, reqTools...)

	sel := strictToolsSelection{all: true}
	require.NoError(t, sel.apply(std))
	require.NoError(t, sel.applyBeta(beta))
	assert.Equal(t, toolStrictFlags(t, std), betaStrictFlags(t, beta))
	assert.Equal(t, map[string]bool{"good": true, "open": false, "bad_kw": false}, betaStrictFlags(t, beta))

	named := strictToolsSelection{names: []string{"bad_kw"}}
	stdErr := named.apply(std)
	betaErr := named.applyBeta(beta)
	require.Error(t, stdErr)
	require.Error(t, betaErr)
	assert.Equal(t, stdErr.Error(), betaErr.Error())
}

func TestStrictTools_IgnoresNonCustomTools(t *testing.T) {
	t.Parallel()

	params := []anthropic.ToolUnionParam{
		{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{}},
	}
	require.NoError(t, strictToolsSelection{all: true}.apply(params))
	require.NoError(t, strictToolsSelection{names: []string{"web_search"}}.apply(params))
}
