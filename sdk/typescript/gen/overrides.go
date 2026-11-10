package main

import (
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/docker/docker-agent/pkg/config/latest"
)

// Each override mirrors the MarshalJSON of its type;
// TestSchemasMatchEncodedValues checks sample values against them.

func durationSchema(*builder) (*jsonschema.Schema, error) {
	return &jsonschema.Schema{Type: "string", Description: `Go duration string such as "1m30s"; empty when unset.`}, nil
}

func streamIdleTimeoutSchema(*builder) (*jsonschema.Schema, error) {
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
		{Type: "string", Description: "Positive Go duration."},
		{Type: "integer", Enum: []any{-1}, Description: "Disable the idle timer."},
	}}, nil
}

func instructionFilesSchema(*builder) (*jsonschema.Schema, error) {
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
		{Type: "string"},
		{Type: "array", Items: &jsonschema.Schema{Type: "string"}},
	}}, nil
}

func skillsConfigSchema(b *builder) (*jsonschema.Schema, error) {
	inline, err := b.ref(reflect.TypeFor[latest.InlineSkill]())
	if err != nil {
		return nil, err
	}
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
		{Type: "boolean"},
		{Type: "array", Items: &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Type: "string"}, inline}}},
	}}, nil
}

func thinkingBudgetSchema(*builder) (*jsonschema.Schema, error) {
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
		{Type: "string", Description: "Effort level."},
		{Type: "integer", Description: "Token budget."},
	}}, nil
}

func taskBudgetSchema(*builder) (*jsonschema.Schema, error) {
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{
		{Type: "integer", Description: "Token budget."},
		{
			Type:          "object",
			Properties:    map[string]*jsonschema.Schema{"type": {Type: "string"}, "total": {Type: "integer"}},
			PropertyOrder: []string{"type", "total"},
			Required:      []string{"type", "total"},
		},
	}}, nil
}

// ragStrategyConfigSchema flattens strategy-specific parameters next to the
// known keys and writes the database as its path.
func ragStrategyConfigSchema(b *builder) (*jsonschema.Schema, error) {
	chunking, err := b.ref(reflect.TypeFor[latest.RAGChunkingConfig]())
	if err != nil {
		return nil, err
	}
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"type":     {Type: "string"},
			"docs":     {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
			"database": {Type: "string"},
			"chunking": chunking,
			"limit":    {Type: "integer"},
		},
		PropertyOrder:        []string{"type", "docs", "database", "chunking", "limit"},
		AdditionalProperties: &jsonschema.Schema{},
	}, nil
}
