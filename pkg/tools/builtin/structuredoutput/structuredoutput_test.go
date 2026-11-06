package structuredoutput

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/tools"
)

func personConfig() *latest.StructuredOutput {
	return &latest.StructuredOutput{
		Name:        "person_info",
		Description: "Information about a person",
		Mode:        latest.StructuredOutputModeTool,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
				"age":  map[string]any{"type": "integer"},
			},
			"required":             []any{"name", "age"},
			"additionalProperties": false,
		},
	}
}

func TestNew_RejectsNilAndBadSchema(t *testing.T) {
	t.Parallel()

	_, err := New(nil)
	require.Error(t, err)

	_, err = New(&latest.StructuredOutput{
		Name:   "broken",
		Schema: map[string]any{"type": 42},
	})
	require.ErrorContains(t, err, "schema")
}

// TestNew_OnlyLocalRefsAllowed pins the security contract: anything but a
// same-document fragment (network, filesystem or cross-document access)
// must be rejected before compilation ever sees the schema.
func TestNew_OnlyLocalRefsAllowed(t *testing.T) {
	t.Parallel()

	schemaWithRef := func(ref any) map[string]any {
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"$ref": ref},
			},
		}
	}

	tests := []struct {
		name    string
		ref     any
		wantErr string
	}{
		{
			name:    "http ref rejected",
			ref:     "http://127.0.0.1:1/schema.json",
			wantErr: "non-local $ref",
		},
		{
			name:    "file ref rejected",
			ref:     "file:///etc/passwd",
			wantErr: "non-local $ref",
		},
		{
			name:    "relative document ref rejected",
			ref:     "other.json#",
			wantErr: "non-local $ref",
		},
		{
			name:    "non-string ref rejected",
			ref:     42,
			wantErr: "$ref must be a string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(&latest.StructuredOutput{Name: "refs", Schema: schemaWithRef(tt.ref)})
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestNew_LocalRefIsAcceptedAndResolved proves same-document fragments stay
// usable: a #/$defs/... ref compiles and enforces the referenced subschema,
// including when nested inside arrays (allOf).
func TestNew_LocalRefIsAcceptedAndResolved(t *testing.T) {
	t.Parallel()

	ot, err := New(&latest.StructuredOutput{
		Name: "local_refs",
		Schema: map[string]any{
			"type":  "object",
			"$defs": map[string]any{"answer": map[string]any{"type": "string"}},
			"properties": map[string]any{
				"answer": map[string]any{
					"allOf": []any{map[string]any{"$ref": "#/$defs/answer"}},
				},
			},
			"required": []any{"answer"},
		},
	})
	require.NoError(t, err)

	got, err := ot.Validate(`{"answer":"hi"}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"answer":"hi"}`, got)

	_, err = ot.Validate(`{"answer":42}`)
	require.ErrorContains(t, err, "local_refs")
}

func TestNew_SchemaDrafts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		schema  string
		wantErr string
	}{
		{name: "no $schema", schema: ""},
		{name: "draft-07", schema: "http://json-schema.org/draft-07/schema#"},
		{name: "draft 2020-12", schema: "https://json-schema.org/draft/2020-12/schema"},
		{name: "draft-04 rejected at load", schema: "http://json-schema.org/draft-04/schema#", wantErr: "unsupported $schema"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			schema := map[string]any{
				"type":                 "object",
				"definitions":          map[string]any{"n": map[string]any{"type": "integer"}},
				"properties":           map[string]any{"n": map[string]any{"$ref": "#/definitions/n"}},
				"required":             []any{"n"},
				"additionalProperties": false,
			}
			if tt.schema != "" {
				schema["$schema"] = tt.schema
			}
			ot, err := New(&latest.StructuredOutput{Name: "drafts", Schema: schema})
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)

			_, err = ot.Validate(`{"n":1}`)
			require.NoError(t, err)
			_, err = ot.Validate(`{"n":1.5}`)
			require.ErrorContains(t, err, "drafts")
			_, err = ot.Validate(`{"n":1,"x":2}`)
			require.ErrorContains(t, err, "drafts")
		})
	}
}

func TestDefinition_UsesConfiguredSchemaAsParameters(t *testing.T) {
	t.Parallel()

	cfg := personConfig()
	ot, err := New(cfg)
	require.NoError(t, err)

	def := ot.Definition()
	assert.Equal(t, ToolName, def.Name)
	assert.Equal(t, "structured_output", def.Category)
	assert.Contains(t, def.Description, cfg.Name)
	assert.Contains(t, def.Description, cfg.Description)
	require.NotNil(t, def.Handler)

	params, ok := def.Parameters.(map[string]any)
	require.True(t, ok, "Parameters must be exactly the configured schema object")
	assert.Equal(t, cfg.Schema, params)
}

func TestValidate(t *testing.T) {
	t.Parallel()

	ot, err := New(personConfig())
	require.NoError(t, err)

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{
			name: "valid compacted",
			in:   `{"name":"Ada","age":36}`,
			want: `{"name":"Ada","age":36}`,
		},
		{
			name: "valid is canonicalized",
			in:   "{\n  \"name\": \"Ada\",\n  \"age\": 36\n}",
			want: `{"name":"Ada","age":36}`,
		},
		{
			name:    "empty arguments fail required",
			in:      "",
			wantErr: "person_info",
		},
		{
			name:    "invalid JSON",
			in:      `{"name": "Ada",`,
			wantErr: "not valid JSON",
		},
		{
			name:    "schema violation names the field",
			in:      `{"name":"Ada","age":"old"}`,
			wantErr: "age",
		},
		{
			name:    "additional property rejected",
			in:      `{"name":"Ada","age":36,"extra":true}`,
			wantErr: "person_info",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ot.Validate(tt.in)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHandler_ReturnsErrorResultInsteadOfError(t *testing.T) {
	t.Parallel()

	ot, err := New(personConfig())
	require.NoError(t, err)
	def := ot.Definition()

	res, err := def.Handler(t.Context(), tools.ToolCall{
		ID:       "call_1",
		Function: tools.FunctionCall{Name: ToolName, Arguments: `{"name":"Ada"}`},
	}, tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.IsError)
	assert.Contains(t, res.Output, "Structured output rejected")
	assert.Contains(t, res.Output, ToolName)

	res, err = def.Handler(t.Context(), tools.ToolCall{
		ID:       "call_2",
		Function: tools.FunctionCall{Name: ToolName, Arguments: ` {"name":"Ada", "age": 36} `},
	}, tools.NopRuntime{})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.IsError)
	assert.JSONEq(t, `{"name":"Ada","age":36}`, res.Output)
	var compacted bytes.Buffer
	require.NoError(t, json.Compact(&compacted, []byte(res.Output)))
	assert.Equal(t, compacted.String(), res.Output, "handler output must be compacted")
}
