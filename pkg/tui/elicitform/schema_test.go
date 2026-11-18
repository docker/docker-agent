package elicitform

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		schema         any
		expectedFields []Field
	}{
		{
			name:           "nil schema",
			schema:         nil,
			expectedFields: nil,
		},
		{
			name:           "empty schema",
			schema:         map[string]any{},
			expectedFields: nil,
		},
		{
			name: "schema with string property",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"username": map[string]any{
						"type":        "string",
						"description": "Your username",
					},
				},
			},
			expectedFields: []Field{
				{
					Name:        "username",
					Type:        "string",
					Description: "Your username",
					Required:    false,
				},
			},
		},
		{
			name: "schema with required fields",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"email": map[string]any{
						"type":        "string",
						"description": "Your email address",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Your name",
					},
				},
				"required": []any{"email"},
			},
			expectedFields: []Field{
				{
					Name:        "email",
					Type:        "string",
					Description: "Your email address",
					Required:    true,
				},
				{
					Name:        "name",
					Type:        "string",
					Description: "Your name",
					Required:    false,
				},
			},
		},
		{
			name: "schema with boolean property",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remember_me": map[string]any{
						"type":        "boolean",
						"description": "Remember this device",
						"default":     true,
					},
				},
			},
			expectedFields: []Field{
				{
					Name:        "remember_me",
					Type:        "boolean",
					Description: "Remember this device",
					Required:    false,
					Default:     true,
				},
			},
		},
		{
			name: "schema with number property",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{
						"type":        "integer",
						"description": "Number of items",
						"default":     float64(10),
					},
				},
			},
			expectedFields: []Field{
				{
					Name:        "count",
					Type:        "integer",
					Description: "Number of items",
					Required:    false,
					Default:     float64(10),
				},
			},
		},
		{
			name: "schema with enum property",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"color": map[string]any{
						"type":        "string",
						"description": "Choose a color",
						"enum":        []any{"red", "green", "blue"},
					},
				},
			},
			expectedFields: []Field{
				{
					Name:        "color",
					Type:        "enum",
					Description: "Choose a color",
					Required:    false,
					EnumValues:  []string{"red", "green", "blue"},
				},
			},
		},
		{
			name: "schema with multiple properties sorted",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"zebra": map[string]any{
						"type": "string",
					},
					"apple": map[string]any{
						"type": "string",
					},
					"required_field": map[string]any{
						"type": "string",
					},
				},
				"required": []any{"required_field"},
			},
			expectedFields: []Field{
				{
					Name:     "required_field",
					Type:     "string",
					Required: true,
				},
				{
					Name:     "apple",
					Type:     "string",
					Required: false,
				},
				{
					Name:     "zebra",
					Type:     "string",
					Required: false,
				},
			},
		},
		{
			name: "schema with property title used for display",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"user_email": map[string]any{
						"type":        "string",
						"title":       "Email Address",
						"description": "Your email",
					},
				},
			},
			expectedFields: []Field{
				{
					Name:        "user_email",
					Title:       "Email Address",
					Type:        "string",
					Description: "Your email",
					Required:    false,
				},
			},
		},
		// Primitive schema tests
		{
			name: "primitive string schema with title",
			schema: map[string]any{
				"type":        "string",
				"title":       "Display Name",
				"description": "Your display name",
				"minLength":   float64(3),
				"maxLength":   float64(50),
				"default":     "user@example.com",
			},
			expectedFields: []Field{
				{
					Name:        "Display Name",
					Title:       "Display Name",
					Type:        "string",
					Description: "Your display name",
					Required:    true, // primitive schemas are implicitly required
					Default:     "user@example.com",
					MinLength:   3,
					MaxLength:   50,
				},
			},
		},
		{
			name: "primitive string schema without title",
			schema: map[string]any{
				"type":        "string",
				"description": "Enter a value",
			},
			expectedFields: []Field{
				{
					Name:        "value", // fallback name
					Type:        "string",
					Description: "Enter a value",
					Required:    true,
				},
			},
		},
		{
			name: "primitive boolean schema",
			schema: map[string]any{
				"type":    "boolean",
				"title":   "Accept Terms",
				"default": true,
			},
			expectedFields: []Field{
				{
					Name:     "Accept Terms",
					Title:    "Accept Terms",
					Type:     "boolean",
					Required: true,
					Default:  true,
				},
			},
		},
		{
			name: "primitive integer schema",
			schema: map[string]any{
				"type":    "integer",
				"title":   "Age",
				"default": float64(25),
			},
			expectedFields: []Field{
				{
					Name:     "Age",
					Title:    "Age",
					Type:     "integer",
					Required: true,
					Default:  float64(25),
				},
			},
		},
		{
			name: "schema with titled single select",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"color": map[string]any{
						"type": "string",
						"oneOf": []any{
							map[string]any{"const": "r", "title": "Red", "description": "Warm"},
							map[string]any{"const": "b", "title": "Blue"},
							map[string]any{"const": "g"},
						},
					},
				},
			},
			expectedFields: []Field{
				{
					Name:             "color",
					Type:             "enum",
					EnumValues:       []string{"r", "b", "g"},
					EnumTitles:       []string{"Red", "Blue", "g"},
					EnumDescriptions: []string{"Warm", "", ""},
				},
			},
		},
		{
			name: "schema with multiselect enum",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pets": map[string]any{
						"type":     "array",
						"title":    "Pets",
						"items":    map[string]any{"type": "string", "enum": []any{"cat", "dog", "fish"}},
						"default":  []any{"dog"},
						"minItems": float64(1),
						"maxItems": float64(2),
					},
				},
			},
			expectedFields: []Field{
				{
					Name:       "pets",
					Title:      "Pets",
					Type:       "multiselect",
					EnumValues: []string{"cat", "dog", "fish"},
					Default:    []any{"dog"},
					MinItems:   1,
					MaxItems:   2,
				},
			},
		},
		{
			name: "schema with titled multiselect",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pets": map[string]any{
						"type": "array",
						"items": map[string]any{"anyOf": []any{
							map[string]any{"const": "cat", "title": "Cat", "description": "Purrs"},
							map[string]any{"const": "dog", "title": "Dog"},
						}},
					},
				},
			},
			expectedFields: []Field{
				{
					Name:             "pets",
					Type:             "multiselect",
					EnumValues:       []string{"cat", "dog"},
					EnumTitles:       []string{"Cat", "Dog"},
					EnumDescriptions: []string{"Purrs", ""},
				},
			},
		},
		{
			name: "array without choices stays a text field",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			},
			expectedFields: []Field{
				{Name: "tags", Type: "array"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fields := ParseSchema(tt.schema)
			assert.Equal(t, tt.expectedFields, fields)
		})
	}
}
