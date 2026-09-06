package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"

	hclconv "github.com/docker/docker-agent/pkg/config/hcl"
	"github.com/docker/docker-agent/pkg/config/latest"
)

// schemaFile is the path to the JSON schema file relative to the repo root.
const schemaFile = "../../agent-schema.json"

func TestJsonSchemaWorksForExamples(t *testing.T) {
	t.Parallel()

	schemaFile, err := os.ReadFile(schemaFile)
	require.NoError(t, err)

	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schemaFile))
	require.NoError(t, err)

	for _, file := range collectExamples(t) {
		t.Run(file, func(t *testing.T) {
			t.Parallel()

			buf, err := os.ReadFile(file)
			require.NoError(t, err)

			// HCL examples are converted to their YAML equivalent before
			// being validated against the JSON schema, since the schema
			// describes the YAML/JSON representation only.
			if filepath.Ext(file) == ".hcl" {
				buf, err = hclconv.ToYAML(buf, file)
				require.NoError(t, err)
			}

			var rawJSON any
			err = yaml.Unmarshal(buf, &rawJSON)
			require.NoError(t, err)

			result, err := schema.Validate(gojsonschema.NewRawLoader(rawJSON))
			require.NoError(t, err)
			assert.True(t, result.Valid(), "Example %s does not match schema: %v", file, result.Errors())
		})
	}
}

// TestJsonSchemaRejectsZeroAgents pins the runtime's "at least one agent"
// invariant (enforced by validateConfig) at the schema level too. Both the
// root-level "required": ["agents"] and the "agents"."minProperties": 1
// constraints are needed: without either one, one of these shapes would
// pass schema validation despite being rejected at runtime.
func TestJsonSchemaRejectsZeroAgents(t *testing.T) {
	t.Parallel()

	schemaBytes, err := os.ReadFile(schemaFile)
	require.NoError(t, err)

	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schemaBytes))
	require.NoError(t, err)

	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "agents omitted entirely",
			yaml: `version: "12"
`,
		},
		{
			name: "agents is an empty map",
			yaml: `version: "12"
agents: {}
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rawJSON any
			require.NoError(t, yaml.Unmarshal([]byte(tt.yaml), &rawJSON))

			result, err := schema.Validate(gojsonschema.NewRawLoader(rawJSON))
			require.NoError(t, err)
			assert.False(t, result.Valid(), "expected schema to reject config with %s", tt.name)
		})
	}
}

// TestJsonSchemaRejectsMalformedCapabilities pins that the capabilities
// override fields are strictly boolean: a non-boolean value (e.g. a string)
// for image/pdf/audio/video must fail schema validation rather than silently
// coercing, since a malformed override would otherwise reach the provider
// and surface as a confusing runtime error instead of an early config error.
func TestJsonSchemaRejectsMalformedCapabilities(t *testing.T) {
	t.Parallel()

	schemaBytes, err := os.ReadFile(schemaFile)
	require.NoError(t, err)

	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schemaBytes))
	require.NoError(t, err)

	const base = `version: "15"
models:
  m:
    provider: openai
    model: gpt-4o
    capabilities:
%s
agents:
  root:
    model: m
    instruction: hi
`

	tests := []struct {
		name  string
		field string
	}{
		{name: "image not boolean", field: "      image: \"yes\"\n"},
		{name: "pdf not boolean", field: "      pdf: \"yes\"\n"},
		{name: "audio not boolean", field: "      audio: \"yes\"\n"},
		{name: "video not boolean", field: "      video: \"yes\"\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rawJSON any
			require.NoError(t, yaml.Unmarshal(fmt.Appendf(nil, base, tt.field), &rawJSON))

			result, err := schema.Validate(gojsonschema.NewRawLoader(rawJSON))
			require.NoError(t, err)
			assert.False(t, result.Valid(), "expected schema to reject a non-boolean %s", tt.name)
		})
	}
}

// TestJsonSchemaAcceptsAudioVideoCapabilities confirms the audio/video
// capability override fields validate against the schema, both individually
// and together with image/pdf, mirroring how the runtime resolves them.
func TestJsonSchemaAcceptsAudioVideoCapabilities(t *testing.T) {
	t.Parallel()

	schemaBytes, err := os.ReadFile(schemaFile)
	require.NoError(t, err)

	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schemaBytes))
	require.NoError(t, err)

	const in = `version: "15"
models:
  m:
    provider: openai
    model: gpt-4o
    capabilities:
      image: true
      pdf: true
      audio: true
      video: true
agents:
  root:
    model: m
    instruction: hi
`

	var rawJSON any
	require.NoError(t, yaml.Unmarshal([]byte(in), &rawJSON))

	result, err := schema.Validate(gojsonschema.NewRawLoader(rawJSON))
	require.NoError(t, err)
	assert.True(t, result.Valid(), "expected schema to accept audio/video capabilities: %v", result.Errors())
}

// TestJsonSchemaRejectsMalformedOutputCapabilities mirrors
// TestJsonSchemaRejectsMalformedCapabilities for output_capabilities.image: a
// non-boolean value must fail schema validation rather than silently
// coercing.
func TestJsonSchemaRejectsMalformedOutputCapabilities(t *testing.T) {
	t.Parallel()

	schemaBytes, err := os.ReadFile(schemaFile)
	require.NoError(t, err)

	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schemaBytes))
	require.NoError(t, err)

	const in = `version: "15"
models:
  m:
    provider: google
    model: gemini-2.5-flash-image
    output_capabilities:
      image: "yes"
agents:
  root:
    model: m
    instruction: hi
`

	var rawJSON any
	require.NoError(t, yaml.Unmarshal([]byte(in), &rawJSON))

	result, err := schema.Validate(gojsonschema.NewRawLoader(rawJSON))
	require.NoError(t, err)
	assert.False(t, result.Valid(), "expected schema to reject a non-boolean output_capabilities.image")
}

// TestJsonSchemaAcceptsOutputCapabilities confirms output_capabilities.image
// validates against the schema, both true and false, and that an unknown
// key under output_capabilities is rejected (additionalProperties: false),
// mirroring the strictness of CapabilitiesConfig.
func TestJsonSchemaAcceptsOutputCapabilities(t *testing.T) {
	t.Parallel()

	schemaBytes, err := os.ReadFile(schemaFile)
	require.NoError(t, err)

	schema, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schemaBytes))
	require.NoError(t, err)

	const base = `version: "15"
models:
  m:
    provider: google
    model: gemini-2.5-flash-image
    output_capabilities:
%s
agents:
  root:
    model: m
    instruction: hi
`

	tests := []struct {
		name  string
		field string
		valid bool
	}{
		{name: "image true", field: "      image: true\n", valid: true},
		{name: "image false", field: "      image: false\n", valid: true},
		{name: "image omitted (empty block)", field: "      {}\n", valid: true},
		{name: "unknown key", field: "      video: true\n", valid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rawJSON any
			require.NoError(t, yaml.Unmarshal(fmt.Appendf(nil, base, tt.field), &rawJSON))

			result, err := schema.Validate(gojsonschema.NewRawLoader(rawJSON))
			require.NoError(t, err)
			assert.Equal(t, tt.valid, result.Valid(), "output_capabilities %s: %v", tt.name, result.Errors())
		})
	}
}

// TestSchemaMatchesGoTypes verifies that every JSON-tagged field in the Go
// config structs has a corresponding property in agent-schema.json (and
// vice-versa). This prevents the schema from silently drifting out of sync
// with the Go types.
func TestSchemaMatchesGoTypes(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(schemaFile)
	require.NoError(t, err, "failed to read schema file – run this test from the repo root")

	var root jsonSchema
	require.NoError(t, json.Unmarshal(data, &root))

	// mapping maps a JSON Schema definition name (or pseudo-name for inline
	// schemas) to the corresponding Go type. For top-level definitions that
	// live in the "definitions" section of the schema we use their exact
	// name. For schemas inlined inside a parent property we use
	// "Parent.property" as the key.
	type entry struct {
		goType     reflect.Type
		schemaDef  jsonSchema
		schemaName string // human-readable name for error messages
	}

	entries := []entry{
		// Top-level Config
		{reflect.TypeFor[latest.Config](), root, "Config (top-level)"},
	}

	// Definitions that map 1:1 to a Go struct.
	definitionMap := map[string]reflect.Type{
		"AgentConfig":           reflect.TypeFor[latest.AgentConfig](),
		"FallbackConfig":        reflect.TypeFor[latest.FallbackConfig](),
		"HarnessConfig":         reflect.TypeFor[latest.HarnessConfig](),
		"ModelConfig":           reflect.TypeFor[latest.ModelConfig](),
		"Metadata":              reflect.TypeFor[latest.Metadata](),
		"ProviderConfig":        reflect.TypeFor[latest.ProviderConfig](),
		"Toolset":               reflect.TypeFor[latest.Toolset](),
		"Remote":                reflect.TypeFor[latest.Remote](),
		"ScriptShellToolConfig": reflect.TypeFor[latest.ScriptShellToolConfig](),
		"PostEditConfig":        reflect.TypeFor[latest.PostEditConfig](),
		"PermissionsConfig":     reflect.TypeFor[latest.PermissionsConfig](),
		"RuntimeDefaults":       reflect.TypeFor[latest.RuntimeDefaults](),
		"BudgetConfig":          reflect.TypeFor[latest.BudgetConfig](),
		"HooksConfig":           reflect.TypeFor[latest.HooksConfig](),
		"HookMatcherConfig":     reflect.TypeFor[latest.HookMatcherConfig](),
		"HookDefinition":        reflect.TypeFor[latest.HookDefinition](),
		"RoutingRule":           reflect.TypeFor[latest.RoutingRule](),
		"ApiConfig":             reflect.TypeFor[latest.APIToolConfig](),
	}

	for name, goType := range definitionMap {
		def, ok := root.Definitions[name]
		require.True(t, ok, "schema definition %q not found", name)
		entries = append(entries, entry{goType, def, name})
	}

	// Inline schemas that don't have their own top-level definition but are
	// nested inside a parent property.
	type inlineEntry struct {
		goType reflect.Type
		// path navigates from a schema definition to the inline object,
		// e.g. []string{"RAGConfig", "results"} → definitions.RAGConfig.properties.results
		path []string
		name string
	}

	inlines := []inlineEntry{
		{reflect.TypeFor[latest.StructuredOutput](), []string{"AgentConfig", "structured_output"}, "StructuredOutput (AgentConfig.structured_output)"},
		{reflect.TypeFor[latest.RAGConfig](), []string{"RAGConfig"}, "RAGConfig"},
		{reflect.TypeFor[latest.RAGToolConfig](), []string{"RAGConfig", "tool"}, "RAGToolConfig (RAGConfig.tool)"},
		{reflect.TypeFor[latest.RAGResultsConfig](), []string{"RAGConfig", "results"}, "RAGResultsConfig (RAGConfig.results)"},
		{reflect.TypeFor[latest.RAGFusionConfig](), []string{"RAGConfig", "results", "fusion"}, "RAGFusionConfig (RAGConfig.results.fusion)"},
		{reflect.TypeFor[latest.RAGRerankingConfig](), []string{"RAGConfig", "results", "reranking"}, "RAGRerankingConfig (RAGConfig.results.reranking)"},
		{reflect.TypeFor[latest.RAGChunkingConfig](), []string{"RAGConfig", "strategies", "*", "chunking"}, "RAGChunkingConfig (RAGConfig.strategies[].chunking)"},
	}

	for _, il := range inlines {
		def := navigateSchema(t, root, il.path)
		entries = append(entries, entry{il.goType, def, il.name})
	}

	// Now compare each entry.
	for _, e := range entries {
		goFields := structJSONFields(e.goType)
		schemaProps := schemaProperties(e.schemaDef)

		missingInSchema := diff(goFields, schemaProps)
		missingInGo := diff(schemaProps, goFields)

		assert.Empty(t, sortedKeys(missingInSchema),
			"%s: Go struct has JSON fields not present in the schema", e.schemaName)
		assert.Empty(t, sortedKeys(missingInGo),
			"%s: schema has properties not present in the Go struct", e.schemaName)
	}
}

// TestHCLBlockRulesCoverSchemaMaps verifies that every top-level property in
// agent-schema.json shaped like a keyed map (an object with
// additionalProperties) has a matching modeMapByLabel rule registered in the
// HCL converter. Without this, adding a new top-level section to the schema
// would silently produce awkward HCL ergonomics (e.g. tool "x" {} mapping to
// tool.x instead of tools.x).
func TestHCLBlockRulesCoverSchemaMaps(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(schemaFile)
	require.NoError(t, err)

	var root jsonSchema
	require.NoError(t, json.Unmarshal(data, &root))

	covered := hclconv.LabelKeyedMapOutKeys()
	for name, prop := range root.Properties {
		if prop.AdditionalProperties == nil {
			continue
		}
		assert.True(t, covered[name],
			"top-level schema map %q has no matching modeMapByLabel rule in pkg/config/hcl", name)
	}
}

// jsonSchema mirrors the subset of JSON Schema we need for comparison.
type jsonSchema struct {
	Properties           map[string]jsonSchema `json:"properties,omitempty"`
	Definitions          map[string]jsonSchema `json:"definitions,omitempty"`
	Ref                  string                `json:"$ref,omitempty"`
	Items                *jsonSchema           `json:"items,omitempty"`
	AdditionalProperties any                   `json:"additionalProperties,omitempty"`
}

// resolveRef follows a $ref like "#/definitions/Foo" and returns the
// referenced schema. When no $ref is present it returns the receiver unchanged.
func (s jsonSchema) resolveRef(root jsonSchema) jsonSchema {
	if s.Ref == "" {
		return s
	}
	const prefix = "#/definitions/"
	if !strings.HasPrefix(s.Ref, prefix) {
		return s
	}
	name := strings.TrimPrefix(s.Ref, prefix)
	if def, ok := root.Definitions[name]; ok {
		return def
	}
	return s
}

// structJSONFields returns the set of JSON property names declared on a Go
// struct type via `json:"<name>,…"` tags. Fields tagged with `json:"-"` are
// excluded. It recurses into anonymous (embedded) struct fields so that
// promoted fields are included.
func structJSONFields(t reflect.Type) map[string]bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	fields := make(map[string]bool)
	for f := range t.Fields() {
		// Recurse into anonymous (embedded) structs.
		if f.Anonymous {
			maps.Copy(fields, structJSONFields(f.Type))
			continue
		}

		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			fields[name] = true
		}
	}
	return fields
}

// schemaProperties returns the set of property names from a JSON schema
// definition. It does NOT follow $ref on individual properties – it only
// looks at the top-level "properties" map.
func schemaProperties(def jsonSchema) map[string]bool {
	props := make(map[string]bool, len(def.Properties))
	for k := range def.Properties {
		props[k] = true
	}
	return props
}

// navigateSchema walks from a top-level definition through nested properties.
// path[0] is the definition name; subsequent elements are property names.
// The special element "*" dereferences an array's "items" schema.
func navigateSchema(t *testing.T, root jsonSchema, path []string) jsonSchema {
	t.Helper()
	require.NotEmpty(t, path)

	cur, ok := root.Definitions[path[0]]
	require.True(t, ok, "definition %q not found", path[0])

	// Resolve top-level $ref if present.
	cur = cur.resolveRef(root)

	for _, segment := range path[1:] {
		if segment == "*" {
			require.NotNil(t, cur.Items, "expected items schema at %v", path)
			cur = *cur.Items
			cur = cur.resolveRef(root)
			continue
		}
		prop, ok := cur.Properties[segment]
		require.True(t, ok, "property %q not found at %v", segment, path)
		prop = prop.resolveRef(root)
		cur = prop
	}
	return cur
}

// diff returns keys present in a but not in b.
func diff(a, b map[string]bool) map[string]bool {
	d := make(map[string]bool)
	for k := range a {
		if !b[k] {
			d[k] = true
		}
	}
	return d
}
