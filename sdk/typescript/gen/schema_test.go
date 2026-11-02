package main

import (
	"encoding/json"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

var loadSource = sync.OnceValues(func() (*goSource, error) {
	return loadGoSource(sortedKeys(defaultConfig().modules))
})

func generated(t *testing.T) []*def {
	t.Helper()
	src, err := loadSource()
	require.NoError(t, err)
	defs, err := build(defaultConfig(), src)
	require.NoError(t, err)
	return defs
}

func TestGenerateIsDeterministic(t *testing.T) {
	t.Parallel()

	first, err := generate(t.Context(), defaultConfig(), t.TempDir())
	require.NoError(t, err)
	second, err := generate(t.Context(), defaultConfig(), t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestEveryAPITypeIsGenerated(t *testing.T) {
	t.Parallel()

	const apiPkg = "github.com/docker/docker-agent/pkg/api"
	src, err := loadSource()
	require.NoError(t, err)

	generatedTypes := map[string]bool{}
	for _, d := range generated(t) {
		if d.goType != nil && d.goType.PkgPath() == apiPkg {
			generatedTypes[d.goType.Name()] = true
		}
	}
	for _, name := range src.exportedStructs(apiPkg) {
		assert.Truef(t, generatedTypes[name], "api.%s is not generated: add it to the roots in config.go", name)
	}
}

func TestEveryEventIsInTheUnion(t *testing.T) {
	t.Parallel()

	defs := generated(t)
	var union *def
	for _, d := range defs {
		if d.name == unionName {
			union = d
		}
	}
	require.NotNil(t, union)
	assert.Len(t, union.schema.OneOf, len(runtime.EventTypes()))
}

// Zero values exercise every field; overrides get samples of each JSON shape
// their MarshalJSON produces.
func TestSchemasMatchEncodedValues(t *testing.T) {
	t.Parallel()

	cfg := defaultConfig()
	defs := generated(t)
	validate := validator(t, defs)
	src, err := loadSource()
	require.NoError(t, err)

	eventKeys := map[reflect.Type]string{}
	for key, typ := range cfg.events {
		eventKeys[typ] = key
	}
	for _, d := range defs {
		// A zero Toolset has no type; samples below cover every variant.
		if d.goType == nil || nilable(d.goType) || d.goType == reflect.TypeFor[latest.Toolset]() {
			continue
		}
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			v := reflect.New(d.goType)
			fillEnums(v.Elem(), cfg, src)
			if key, ok := eventKeys[d.goType]; ok {
				v.Elem().FieldByName("Type").SetString(key)
			}
			validate(t, d.name, v.Interface())
		})
	}

	var rag latest.RAGStrategyConfig
	require.NoError(t, json.Unmarshal([]byte(`{"type":"bm25","docs":["a.md"],"database":"rag.db","chunking":{"size":10},"limit":3,"k1":1.2}`), &rag))

	sub := session.New(session.WithTitle("sub"))
	sess := session.New(session.WithUserMessage("hi"), session.WithMessages([]session.Item{{SubSession: sub}}))

	var toolsets []any
	for _, typ := range cfg.toolsetTypes {
		toolsets = append(toolsets, latest.Toolset{Type: typ})
	}
	toolsets = append(toolsets,
		latest.Toolset{Type: "shell", SudoAskpass: new(true), Env: map[string]string{"k": "v"}},
		latest.Toolset{Type: "mcp", Command: "server", Args: []string{"--stdio"}, Lifecycle: &latest.LifecycleConfig{}},
	)

	samples := map[string][]any{
		"Toolset":           toolsets,
		"Duration":          {latest.Duration{}, latest.Duration{Duration: 90 * time.Second}},
		"InstructionFiles":  {latest.InstructionFiles{"a.md"}, latest.InstructionFiles{"a.md", "b.md"}},
		"SkillsConfig":      {latest.SkillsConfig{}, latest.SkillsConfig{Sources: []string{latest.SkillSourceLocal}}, latest.SkillsConfig{Sources: []string{"https://example.com"}, Include: []string{"git"}, Inline: []latest.InlineSkill{{Name: "x", Description: "d", Instructions: "i"}}}},
		"ThinkingBudget":    {latest.ThinkingBudget{Effort: "high"}, latest.ThinkingBudget{Tokens: 1024}},
		"TaskBudget":        {latest.TaskBudget{Total: 5}, latest.TaskBudget{Type: "other", Total: 3}},
		"RAGStrategyConfig": {rag},
		"Session":           {sess},
		"Tool":              {tools.Tool{Name: "t", Parameters: map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{}, "a": map[string]any{}}}}},
		"ChatMessage":       {chat.Message{Role: chat.MessageRoleAssistant, ToolCalls: []tools.ToolCall{{ID: "1", Type: "function"}}}},
		// Unlike fillEnums' values, "" comes from the extra enum values.
		"UpdateSessionSafetyPolicyRequest": {api.UpdateSessionSafetyPolicyRequest{}},
	}
	for name, values := range samples {
		t.Run("sample "+name, func(t *testing.T) {
			t.Parallel()
			for _, v := range values {
				validate(t, name, v)
			}
		})
	}
}

// fillEnums sets enum fields, whose zero value is not one of their
// constants, to their first constant.
func fillEnums(v reflect.Value, cfg *config, src *goSource) {
	t := v.Type()
	if _, ok := cfg.enums[t]; ok {
		v.SetString(src.consts[t.PkgPath()+"."+t.Name()][0])
		return
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		if f := v.Field(i); f.CanSet() {
			fillEnums(f, cfg, src)
		}
	}
}

// validator checks values against the generated schemas, with structs
// closed so that a field the schema misses fails too.
func validator(t *testing.T, defs []*def) func(t *testing.T, name string, v any) {
	t.Helper()
	all := map[string]*jsonschema.Schema{}
	for _, d := range defs {
		all[d.name] = d.schema.CloneSchemas()
		closeObjects(all[d.name])
	}

	return func(t *testing.T, name string, v any) {
		t.Helper()
		require.Contains(t, all, name)
		root := (&jsonschema.Schema{Schema: "https://json-schema.org/draft/2020-12/schema", Ref: "#/$defs/" + name, Defs: all}).CloneSchemas()
		resolved, err := root.Resolve(nil)
		require.NoError(t, err)

		data, err := json.Marshal(v)
		require.NoError(t, err)
		var instance any
		require.NoError(t, json.Unmarshal(data, &instance))
		assert.NoErrorf(t, resolved.Validate(instance), "%s does not match the %s schema", data, name)
	}
}

func closeObjects(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if s.Type == "object" && s.Properties != nil && s.AdditionalProperties == nil {
		s.AdditionalProperties = &jsonschema.Schema{Not: &jsonschema.Schema{}}
	}
	for _, p := range s.Properties {
		closeObjects(p)
	}
	for _, m := range slices.Concat(s.AnyOf, s.OneOf) {
		closeObjects(m)
	}
	closeObjects(s.Items)
	closeObjects(s.AdditionalProperties)
}
