// Package elicitform turns MCP elicitation schemas into form fields and
// validates the user's answers, independent of how a TUI renders them.
package elicitform

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Field types produced by [ParseSchema]. Any other type is entered as text.
const (
	TypeBoolean     = "boolean"
	TypeEnum        = "enum"
	TypeMultiSelect = "multiselect"
	TypeNumber      = "number"
	TypeInteger     = "integer"
	TypeString      = "string"
)

// Field represents a form field extracted from a JSON schema.
type Field struct {
	Name, Title, Type, Description string
	Required                       bool
	EnumValues                     []string
	// EnumTitles and EnumDescriptions parallel EnumValues for titled choices
	// (oneOf / items.anyOf); EnumTitles are shown instead of the values.
	EnumTitles, EnumDescriptions []string
	Default                      any
	MinLength, MaxLength         int
	MinItems, MaxItems           int
	Format, Pattern              string
	Minimum, Maximum             float64
	HasMinimum, HasMaximum       bool
}

// OptionLabels returns what a form shows for each choice.
func (f Field) OptionLabels() []string {
	if len(f.EnumTitles) == len(f.EnumValues) {
		return f.EnumTitles
	}
	return f.EnumValues
}

// IsText reports whether the field is answered by typing.
func (f Field) IsText() bool {
	switch f.Type {
	case TypeBoolean, TypeEnum, TypeMultiSelect:
		return false
	default:
		return true
	}
}

// IsSecret reports whether the field's value must not be echoed.
func (f Field) IsSecret() bool {
	return f.Format == "password"
}

// Answer is the raw state of one field; only the member matching the
// field's type is read.
type Answer struct {
	Text    string
	Bool    bool
	Choice  int
	Checked []bool
}

// DefaultAnswer seeds a field's answer from its schema default.
func DefaultAnswer(f Field) Answer {
	switch f.Type {
	case TypeBoolean:
		b, _ := f.Default.(bool)
		return Answer{Bool: b}
	case TypeEnum:
		if s, ok := f.Default.(string); ok {
			if i := slices.Index(f.EnumValues, s); i >= 0 {
				return Answer{Choice: i}
			}
		}
		return Answer{}
	case TypeMultiSelect:
		defaults, _ := f.Default.([]any)
		checked := make([]bool, len(f.EnumValues))
		for j, value := range f.EnumValues {
			checked[j] = slices.Contains(defaults, any(value))
		}
		return Answer{Checked: checked}
	default:
		if f.Default == nil {
			return Answer{}
		}
		return Answer{Text: fmt.Sprintf("%v", f.Default)}
	}
}

// Collect validates every answer and builds the elicitation content.
// errs maps field index to a message; first is the first failing index or -1.
func Collect(fields []Field, answer func(i int) Answer) (content map[string]any, errs map[int]string, first int) {
	content = make(map[string]any)
	errs = make(map[int]string)
	first = -1
	for i, f := range fields {
		value, present, errMsg := resolve(f, answer(i))
		if errMsg != "" {
			errs[i] = errMsg
			if first < 0 {
				first = i
			}
			continue
		}
		if present {
			content[f.Name] = value
		}
	}
	return content, errs, first
}

// FreeForm builds the content for a schema-less elicitation answer.
func FreeForm(text string) map[string]any {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return map[string]any{"response": text}
}

func resolve(f Field, a Answer) (value any, present bool, errMsg string) {
	switch f.Type {
	case TypeBoolean:
		return a.Bool, true, ""
	case TypeEnum:
		if a.Choice < 0 || a.Choice >= len(f.EnumValues) {
			if f.Required {
				return nil, false, "Selection required"
			}
			return nil, false, ""
		}
		return f.EnumValues[a.Choice], true, ""
	case TypeMultiSelect:
		selected := []any{}
		for j, checked := range a.Checked {
			if checked && j < len(f.EnumValues) {
				selected = append(selected, f.EnumValues[j])
			}
		}
		if errMsg := validateSelectionCount(len(selected), f); errMsg != "" {
			return nil, false, errMsg
		}
		return selected, true, ""
	default:
		val := strings.TrimSpace(a.Text)
		if val == "" {
			if f.Required {
				return nil, false, "This field is required"
			}
			return nil, false, ""
		}
		// Secrets (e.g. a sudo password) may legitimately carry surrounding
		// whitespace; only the emptiness check uses the trimmed copy.
		if f.IsSecret() {
			val = a.Text
		}
		parsed, errMsg := parseText(val, f)
		if errMsg != "" {
			return nil, false, errMsg
		}
		return parsed, true, ""
	}
}

func parseText(val string, f Field) (any, string) {
	switch f.Type {
	case TypeNumber:
		n, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, "Must be a valid number"
		}
		if errMsg := validateNumber(n, f); errMsg != "" {
			return nil, errMsg
		}
		return n, ""
	case TypeInteger:
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return nil, "Must be a whole number"
		}
		if errMsg := validateNumber(float64(n), f); errMsg != "" {
			return nil, errMsg
		}
		return n, ""
	default:
		if errMsg := validateString(val, f); errMsg != "" {
			return nil, errMsg
		}
		return val, ""
	}
}
