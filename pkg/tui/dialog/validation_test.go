package dialog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetCompiledPattern(t *testing.T) {
	t.Parallel()

	t.Run("compiles and caches valid pattern", func(t *testing.T) {
		t.Parallel()
		pattern := "^test-[0-9]+$"

		// First call should compile and cache
		compiled1, err := getCompiledPattern(pattern)
		require.NoError(t, err)
		assert.NotNil(t, compiled1)

		// Second call should return cached version
		compiled2, err := getCompiledPattern(pattern)
		require.NoError(t, err)
		assert.Same(t, compiled1, compiled2) // Same pointer = cached
	})

	t.Run("returns error for invalid pattern", func(t *testing.T) {
		t.Parallel()
		invalidPattern := "[invalid(regex"

		compiled, err := getCompiledPattern(invalidPattern)
		require.Error(t, err)
		assert.Nil(t, compiled)
	})

	t.Run("compiled pattern works correctly", func(t *testing.T) {
		t.Parallel()
		pattern := "^[A-Z]{3}-[0-9]{4}$"

		compiled, err := getCompiledPattern(pattern)
		require.NoError(t, err)

		assert.True(t, compiled.MatchString("ABC-1234"))
		assert.False(t, compiled.MatchString("abc-1234"))
		assert.False(t, compiled.MatchString("ABCD-1234"))
	})
}

func TestValidateStringField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    string
		field    ElicitationField
		expected bool
	}{
		{
			name:     "valid string no constraints",
			value:    "hello",
			field:    ElicitationField{Type: "string"},
			expected: true,
		},
		{
			name:  "minLength fails",
			value: "hi",
			field: ElicitationField{
				Type:      "string",
				MinLength: 5,
			},
			expected: false,
		},
		{
			name:  "minLength passes",
			value: "hello world",
			field: ElicitationField{
				Type:      "string",
				MinLength: 5,
			},
			expected: true,
		},
		{
			name:  "invalid email format",
			value: "not-an-email",
			field: ElicitationField{
				Type:   "string",
				Format: "email",
			},
			expected: false,
		},
		{
			name:  "valid email format",
			value: "test@example.com",
			field: ElicitationField{
				Type:   "string",
				Format: "email",
			},
			expected: true,
		},
		{
			name:  "invalid uri format",
			value: "not-a-url",
			field: ElicitationField{
				Type:   "string",
				Format: "uri",
			},
			expected: false,
		},
		{
			name:  "valid uri format",
			value: "https://example.com/path",
			field: ElicitationField{
				Type:   "string",
				Format: "uri",
			},
			expected: true,
		},
		{
			name:  "invalid date format",
			value: "2024/01/15",
			field: ElicitationField{
				Type:   "string",
				Format: "date",
			},
			expected: false,
		},
		{
			name:  "valid date format",
			value: "2024-01-15",
			field: ElicitationField{
				Type:   "string",
				Format: "date",
			},
			expected: true,
		},
		{
			name:  "invalid date-time format",
			value: "2024-01-15",
			field: ElicitationField{
				Type:   "string",
				Format: "date-time",
			},
			expected: false,
		},
		{
			name:  "valid date-time format",
			value: "2024-01-15T10:30:00Z",
			field: ElicitationField{
				Type:   "string",
				Format: "date-time",
			},
			expected: true,
		},
		{
			name:  "unknown format is permissive",
			value: "anything",
			field: ElicitationField{
				Type:   "string",
				Format: "custom-unknown-format",
			},
			expected: true,
		},
		{
			name:  "pattern fails",
			value: "abc123",
			field: ElicitationField{
				Type:    "string",
				Pattern: "^[A-Z]+$",
			},
			expected: false,
		},
		{
			name:  "pattern passes",
			value: "ABC",
			field: ElicitationField{
				Type:    "string",
				Pattern: "^[A-Z]+$",
			},
			expected: true,
		},
		{
			name:  "invalid pattern returns false",
			value: "test",
			field: ElicitationField{
				Type:    "string",
				Pattern: "[invalid(regex",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := validateStringFieldWithMessage(tt.value, tt.field) == ""
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestValidateNumberField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    float64
		field    ElicitationField
		expected bool
	}{
		{
			name:     "valid number no constraints",
			value:    42.5,
			field:    ElicitationField{Type: "number"},
			expected: true,
		},
		{
			name:  "minimum fails",
			value: 5,
			field: ElicitationField{
				Type:       "number",
				Minimum:    10,
				HasMinimum: true,
			},
			expected: false,
		},
		{
			name:  "minimum passes",
			value: 15,
			field: ElicitationField{
				Type:       "number",
				Minimum:    10,
				HasMinimum: true,
			},
			expected: true,
		},
		{
			name:  "maximum fails",
			value: 150,
			field: ElicitationField{
				Type:       "number",
				Maximum:    100,
				HasMaximum: true,
			},
			expected: false,
		},
		{
			name:  "maximum passes",
			value: 50,
			field: ElicitationField{
				Type:       "number",
				Maximum:    100,
				HasMaximum: true,
			},
			expected: true,
		},
		{
			name:  "within range",
			value: 50,
			field: ElicitationField{
				Type:       "number",
				Minimum:    10,
				Maximum:    100,
				HasMinimum: true,
				HasMaximum: true,
			},
			expected: true,
		},
		{
			name:  "outside range - below",
			value: 5,
			field: ElicitationField{
				Type:       "number",
				Minimum:    10,
				Maximum:    100,
				HasMinimum: true,
				HasMaximum: true,
			},
			expected: false,
		},
		{
			name:  "outside range - above",
			value: 150,
			field: ElicitationField{
				Type:       "number",
				Minimum:    10,
				Maximum:    100,
				HasMinimum: true,
				HasMaximum: true,
			},
			expected: false,
		},
		{
			name:  "boundary - exact minimum",
			value: 10,
			field: ElicitationField{
				Type:       "number",
				Minimum:    10,
				HasMinimum: true,
			},
			expected: true,
		},
		{
			name:  "boundary - exact maximum",
			value: 100,
			field: ElicitationField{
				Type:       "number",
				Maximum:    100,
				HasMaximum: true,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := validateNumberFieldWithMessage(tt.value, tt.field) == ""
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestValidateFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    string
		format   string
		expected bool
	}{
		{"empty format always valid", "anything", "", true},
		{"valid email", "test@example.com", "email", true},
		{"invalid email - no @", "testexample.com", "email", false},
		{"valid uri with path", "https://example.com/path?query=1", "uri", true},
		{"invalid uri - no scheme", "example.com", "uri", false},
		{"valid date", "2024-12-25", "date", true},
		{"invalid date - wrong format", "25/12/2024", "date", false},
		{"valid datetime", "2024-12-25T14:30:00Z", "date-time", true},
		{"invalid datetime - no time", "2024-12-25", "date-time", false},
		{"unknown format is permissive", "any value", "unknown-format", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := validateFormatWithMessage(tt.value, tt.format) == ""
			assert.Equal(t, tt.expected, result)
		})
	}
}
