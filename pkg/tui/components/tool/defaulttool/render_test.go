package defaulttool

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatValue_String(t *testing.T) {
	t.Parallel()

	result := formatValue("hello")
	assert.Equal(t, "hello", result)
}

func TestFormatValue_SingleElementArray(t *testing.T) {
	t.Parallel()

	result := formatValue([]any{"/src/main/cmd/root/run.go"})
	assert.Equal(t, `["/src/main/cmd/root/run.go"]`, result)
}

func TestFormatValue_MultiElementArray(t *testing.T) {
	t.Parallel()

	result := formatValue([]any{"file1.go", "file2.go", "file3.go"})
	expected := `[
  "file1.go",
  "file2.go",
  "file3.go"
]`
	assert.Equal(t, expected, result)
}

func TestFormatValue_EmptyArray(t *testing.T) {
	t.Parallel()

	result := formatValue([]any{})
	assert.Equal(t, "[]", result)
}

func TestFormatValue_Map(t *testing.T) {
	t.Parallel()

	result := formatValue(map[string]any{"key": "value"})
	expected := `{
  "key": "value"
}`
	assert.Equal(t, expected, result)
}

func TestFormatValue_Number(t *testing.T) {
	t.Parallel()

	result := formatValue(42.0)
	assert.Equal(t, "42", result)
}
