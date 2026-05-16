package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseElicitationSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		schema         any
		expectedFields []ElicitationField
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
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
			expectedFields: []ElicitationField{
				{
					Name:     "Age",
					Title:    "Age",
					Type:     "integer",
					Required: true,
					Default:  float64(25),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fields := parseElicitationSchema(tt.schema)
			assert.Equal(t, tt.expectedFields, fields)
		})
	}
}

func TestNewElicitationDialog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		message          string
		schema           any
		meta             map[string]any
		expectedTitle    string
		hasFreeFormInput bool
	}{
		{
			name:             "simple dialog without fields has response input",
			message:          "Please confirm this action",
			schema:           nil,
			expectedTitle:    "Question",
			hasFreeFormInput: true,
		},
		{
			name:    "dialog with form fields",
			message: "Please enter your credentials",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"username": map[string]any{"type": "string", "description": "Your username"},
					"password": map[string]any{"type": "string", "description": "Your password"},
				},
				"required": []any{"username", "password"},
			},
			expectedTitle:    "Question",
			hasFreeFormInput: false,
		},
		{
			name:             "dialog with custom title from meta",
			message:          "Choose wisely",
			schema:           nil,
			meta:             map[string]any{"cagent/title": "Custom Title"},
			expectedTitle:    "Custom Title",
			hasFreeFormInput: true,
		},
		{
			name:             "dialog with empty meta defaults to Question",
			message:          "What?",
			schema:           nil,
			meta:             map[string]any{},
			expectedTitle:    "Question",
			hasFreeFormInput: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dialog := NewElicitationDialog(tt.message, tt.schema, tt.meta)
			require.NotNil(t, dialog)

			ed, ok := dialog.(*ElicitationDialog)
			require.True(t, ok)
			assert.Equal(t, tt.message, ed.message)
			assert.Equal(t, tt.expectedTitle, ed.title)
			assert.Equal(t, tt.hasFreeFormInput, ed.hasFreeFormInput())
		})
	}
}

func TestElicitationDialog_collectAndValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		schema        any
		setupInputs   func(*ElicitationDialog)
		expectedValid bool
		expectedKeys  []string
	}{
		{
			name:          "no fields",
			schema:        nil,
			expectedValid: true,
		},
		{
			name: "required field empty",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
				"required":   []any{"name"},
			},
			expectedValid: false,
		},
		{
			name: "required field filled",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
				"required":   []any{"name"},
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("test_name") },
			expectedValid: true,
			expectedKeys:  []string{"name"},
		},
		{
			name: "boolean field",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"enabled": map[string]any{"type": "boolean"}},
			},
			setupInputs:   func(d *ElicitationDialog) { d.boolValues[0] = true },
			expectedValid: true,
			expectedKeys:  []string{"enabled"},
		},
		{
			name: "invalid integer",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"count": map[string]any{"type": "integer"}},
				"required":   []any{"count"},
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("not_a_number") },
			expectedValid: false,
		},
		{
			name: "valid integer",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"count": map[string]any{"type": "integer"}},
				"required":   []any{"count"},
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("42") },
			expectedValid: true,
			expectedKeys:  []string{"count"},
		},
		{
			name: "valid enum value",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"color": map[string]any{"type": "string", "enum": []any{"red", "green", "blue"}}},
				"required":   []any{"color"},
			},
			setupInputs:   func(d *ElicitationDialog) { d.enumIndexes[0] = 0 }, // select "red"
			expectedValid: true,
			expectedKeys:  []string{"color"},
		},
		{
			name: "minLength validation fails",
			schema: map[string]any{
				"type":      "string",
				"title":     "Name",
				"minLength": float64(5),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("abc") }, // 3 chars, need 5
			expectedValid: false,
		},
		{
			name: "minLength validation passes",
			schema: map[string]any{
				"type":      "string",
				"title":     "Name",
				"minLength": float64(3),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("abcde") },
			expectedValid: true,
			expectedKeys:  []string{"Name"},
		},
		{
			name: "email format validation fails",
			schema: map[string]any{
				"type":   "string",
				"title":  "Email",
				"format": "email",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("not-an-email") },
			expectedValid: false,
		},
		{
			name: "email format validation passes",
			schema: map[string]any{
				"type":   "string",
				"title":  "Email",
				"format": "email",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("test@example.com") },
			expectedValid: true,
			expectedKeys:  []string{"Email"},
		},
		{
			name: "uri format validation fails",
			schema: map[string]any{
				"type":   "string",
				"title":  "Website",
				"format": "uri",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("not-a-url") },
			expectedValid: false,
		},
		{
			name: "uri format validation passes",
			schema: map[string]any{
				"type":   "string",
				"title":  "Website",
				"format": "uri",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("https://example.com") },
			expectedValid: true,
			expectedKeys:  []string{"Website"},
		},
		{
			name: "date format validation passes",
			schema: map[string]any{
				"type":   "string",
				"title":  "Birthday",
				"format": "date",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("2024-01-15") },
			expectedValid: true,
			expectedKeys:  []string{"Birthday"},
		},
		{
			name: "pattern validation fails",
			schema: map[string]any{
				"type":    "string",
				"title":   "Code",
				"pattern": "^[A-Z]{3}$",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("abc") },
			expectedValid: false,
		},
		{
			name: "pattern validation passes",
			schema: map[string]any{
				"type":    "string",
				"title":   "Code",
				"pattern": "^[A-Z]{3}$",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("ABC") },
			expectedValid: true,
			expectedKeys:  []string{"Code"},
		},
		{
			name: "number minimum validation fails",
			schema: map[string]any{
				"type":    "number",
				"title":   "Age",
				"minimum": float64(18),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("15") },
			expectedValid: false,
		},
		{
			name: "number minimum validation passes",
			schema: map[string]any{
				"type":    "number",
				"title":   "Age",
				"minimum": float64(18),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("25") },
			expectedValid: true,
			expectedKeys:  []string{"Age"},
		},
		{
			name: "number maximum validation fails",
			schema: map[string]any{
				"type":    "integer",
				"title":   "Count",
				"maximum": float64(100),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("150") },
			expectedValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dialog := NewElicitationDialog("test", tt.schema, nil).(*ElicitationDialog)
			if tt.setupInputs != nil {
				tt.setupInputs(dialog)
			}

			content, firstErrorIdx := dialog.collectAndValidate()
			valid := firstErrorIdx < 0
			assert.Equal(t, tt.expectedValid, valid)

			if valid && tt.expectedKeys != nil {
				for _, key := range tt.expectedKeys {
					assert.Contains(t, content, key)
				}
			}
		})
	}
}

func TestElicitationDialog_LongEnumScrolls(t *testing.T) {
	t.Parallel()

	// Build an enum with many values that would overflow a typical terminal.
	enumValues := make([]any, 0, 30)
	for i := range 30 {
		enumValues = append(enumValues, "option-"+strings.Repeat("x", 5)+string(rune('A'+i%26)))
	}

	schema := map[string]any{
		"type":  "string",
		"title": "Pick one",
		"enum":  enumValues,
	}

	dialog := NewElicitationDialog("Choose an option:", schema, nil).(*ElicitationDialog)
	require.Len(t, dialog.fields, 1)
	require.Len(t, dialog.fields[0].EnumValues, 30)

	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	_ = dialog.View() // populate fieldStarts and configure the scrollview

	require.Len(t, dialog.fieldStarts, 1)
	assert.True(t, dialog.scrollview.NeedsScrollbar(), "30 enum options on a 20-row terminal must require scrolling")

	// Selecting an option far down the list must auto-scroll it into view.
	dialog.enumIndexes[0] = 25
	dialog.ensureFocusVisible()
	_ = dialog.View()
	offset := dialog.scrollview.ScrollOffset()
	visEnd := offset + dialog.scrollview.VisibleHeight() - 1
	selectedLine := dialog.fieldStarts[0] + 1 + 25
	assert.GreaterOrEqual(t, selectedLine, offset, "selected option must be at or below scroll offset")
	assert.LessOrEqual(t, selectedLine, visEnd, "selected option must be at or above visible end")
}

func TestElicitationDialog_FieldsBelowFold_AreReachable(t *testing.T) {
	t.Parallel()

	props := map[string]any{}
	required := []any{}
	for i := range 15 {
		name := "field" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		props[name] = map[string]any{"type": "string", "title": name}
		required = append(required, name)
	}
	schema := map[string]any{"type": "object", "properties": props, "required": required}

	dialog := NewElicitationDialog("Fill in the form", schema, nil).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 18})
	_ = dialog.View()

	require.Len(t, dialog.fieldStarts, 15)
	assert.True(t, dialog.scrollview.NeedsScrollbar(), "15 fields on an 18-row terminal must require scrolling")

	// Tab through every field; each one must end up within the scroll viewport.
	for i := range 15 {
		for dialog.currentField != i {
			dialog.moveFocus(1)
		}
		_ = dialog.View()
		offset := dialog.scrollview.ScrollOffset()
		visEnd := offset + dialog.scrollview.VisibleHeight() - 1
		start := dialog.fieldStarts[i]
		assert.GreaterOrEqual(t, start, offset, "field %d label must be at or below scroll offset", i)
		assert.LessOrEqual(t, start, visEnd, "field %d label must be visible", i)
	}
}

func TestElicitationDialog_SmallContent_NoScrollbar(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "title": "Name"},
		},
		"required": []any{"name"},
	}

	dialog := NewElicitationDialog("Enter your name", schema, nil).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_ = dialog.View()

	assert.False(t, dialog.scrollview.NeedsScrollbar(), "small dialogs should not need a scrollbar")
}
