package cli

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestFormatToolCallResponse_Empty(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallResponse(``)

	assert.Equal(t, ` → ()`, formatted)
}

func TestFormatToolCallResponse_Map(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallResponse(`{"text": "hello"}`)

	assert.Equal(t, ` → (text: "hello")`, formatted)
}

func TestFormatToolCallResponse_MultilineString(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallResponse(`{"text": "a\nb"}`)

	assert.Equal(t, ` → (text: "a\nb")`, formatted)
}

func TestFormatToolCallResponse_MapOfEmptyArray(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallResponse(`{"array": []}`)
	assert.Equal(t, ` → (array: [])`, formatted)
}

func TestFormatToolCallResponse_MapOfArray(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallResponse(`{"array": [1,2,3]}`)
	assert.Equal(t, ` → (
  array: [
  1,
  2,
  3
]
)`, formatted)
}

func TestFormatToolCallResponse_PlainText(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallResponse(`Plain Text`)

	assert.Equal(t, ` → "Plain Text"`, formatted)
}

func TestFormatToolCallArguments_Empty(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallArguments(``)

	assert.Equal(t, `()`, formatted)
}

func TestFormatToolCallArguments_Map(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallArguments(`{"first": "hello", "second": 42}`)

	assert.Equal(t, `(
  first: "hello"
  second: 42
)`, formatted)
}

func TestFormatToolCallArguments_MapOfArray(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallArguments(`{"array": [1,2,3]}`)
	assert.Equal(t, `(
  array: [
  1,
  2,
  3
]
)`, formatted)
}

func TestFormatToolCallArguments_MapOfEmptyArray(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallArguments(`{"array": []}`)
	assert.Equal(t, `(array: [])`, formatted)
}

func TestFormatToolCallArguments_MapOfSingleItemArray(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallArguments(`{"array": ["value"]}`)
	assert.Equal(t, `(array: ["value"])`, formatted)
}

func TestFormatToolCallArguments_PlainText(t *testing.T) {
	t.Parallel()
	formatted := formatToolCallArguments(`Plain Text`)

	assert.Equal(t, `(Plain Text)`, formatted)
}
