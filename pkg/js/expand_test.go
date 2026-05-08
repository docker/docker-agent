package js

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExpand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		commands string
		values   map[string]string
		expected string
	}{
		{
			name:     "no placeholder",
			commands: "List all files",
			values:   map[string]string{},
			expected: "List all files",
		},
		{
			name:     "simple substitution",
			commands: "Say hello to ${USER}",
			values:   map[string]string{"USER": "alice"},
			expected: "Say hello to alice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			expander := NewJsExpander(nil)
			result := expander.Expand(t.Context(), tt.commands, tt.values)

			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExpandMap(t *testing.T) {
	t.Parallel()

	expander := NewJsExpander(nil)
	result := expander.ExpandMap(t.Context(), map[string]string{
		"none":   "List all files",
		"simple": "Say hello to ${USER}",
	})

	assert.Equal(t, map[string]string{
		"none":   "List all files",
		"simple": "Say hello to ${USER}", // values is nil, so no expansion
	}, result)
}

func TestExpandMap_Reuse(t *testing.T) {
	t.Parallel()

	expander := NewJsExpander(nil)

	result := expander.ExpandMap(t.Context(), map[string]string{
		"none": "List all files",
	})
	assert.Equal(t, map[string]string{
		"none": "List all files",
	}, result)

	result = expander.ExpandMap(t.Context(), map[string]string{
		"simple": "Say hello to ${USER}",
	})
	assert.Equal(t, map[string]string{
		"simple": "Say hello to ${USER}", // values is nil
	}, result)
}

func TestExpandMap_Empty(t *testing.T) {
	t.Parallel()

	expander := NewJsExpander(nil)
	result := expander.ExpandMap(t.Context(), map[string]string{})

	assert.Empty(t, result)
}

func TestExpandString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template string
		values   map[string]string
		expected string
	}{
		{
			name:     "simple substitution",
			template: "Hello ${name}!",
			values:   map[string]string{"name": "World"},
			expected: "Hello World!",
		},
		{
			name:     "multiple values",
			template: "File: ${path} (chunk ${index})",
			values:   map[string]string{"path": "/foo/bar.go", "index": "0"},
			expected: "File: /foo/bar.go (chunk 0)",
		},
		{
			name:     "backticks in template are preserved",
			template: "Code:\n```\n${content}\n```",
			values:   map[string]string{"content": "func main() {}"},
			expected: "Code:\n```\nfunc main() {}\n```",
		},
		{
			name:     "backticks in value are preserved",
			template: "The code is: ${code}",
			values:   map[string]string{"code": "use `fmt.Println()`"},
			expected: "The code is: use `fmt.Println()`",
		},
		{
			name:     "semantic prompt with code fence",
			template: "Summarize:\n```\n${content}\n```\nBe concise.",
			values:   map[string]string{"content": "package main\n\nfunc main() {\n\tfmt.Println(`hello`)\n}"},
			expected: "Summarize:\n```\npackage main\n\nfunc main() {\n\tfmt.Println(`hello`)\n}\n```\nBe concise.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			expander := NewJsExpander(nil)
			result := expander.Expand(t.Context(), tt.template, tt.values)
			assert.Equal(t, tt.expected, result)
		})
	}
}
