package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

func TestExtractXMLToolCalls_NoBlocks(t *testing.T) {
	calls, textBefore, ok := extractXMLToolCalls("Hello, how can I help?")
	assert.False(t, ok)
	assert.Empty(t, calls)
	assert.Empty(t, textBefore)
}

func TestExtractXMLToolCalls_SingleCall(t *testing.T) {
	content := `<tool_call>
{"name": "get_time", "arguments": {}}
</tool_call>`

	calls, textBefore, ok := extractXMLToolCalls(content)
	require.True(t, ok)
	require.Len(t, calls, 1)
	assert.Equal(t, "get_time", calls[0].Function.Name)
	assert.Equal(t, "{}", calls[0].Function.Arguments)
	assert.Equal(t, "call_0", calls[0].ID)
	assert.Equal(t, tools.ToolType("function"), calls[0].Type)
	assert.Empty(t, textBefore)
}

func TestExtractXMLToolCalls_WithArguments(t *testing.T) {
	content := `<tool_call>
{"name": "search", "arguments": {"query": "docker agent"}}
</tool_call>`

	calls, _, ok := extractXMLToolCalls(content)
	require.True(t, ok)
	require.Len(t, calls, 1)
	assert.Equal(t, "search", calls[0].Function.Name)
	assert.JSONEq(t, `{"query": "docker agent"}`, calls[0].Function.Arguments)
}

func TestExtractXMLToolCalls_TextBefore(t *testing.T) {
	content := "I'll search for that.\n<tool_call>\n{\"name\": \"search\", \"arguments\": {\"q\": \"go\"}}\n</tool_call>"

	calls, textBefore, ok := extractXMLToolCalls(content)
	require.True(t, ok)
	assert.Equal(t, "I'll search for that.", textBefore)
	require.Len(t, calls, 1)
	assert.Equal(t, "search", calls[0].Function.Name)
}

func TestExtractXMLToolCalls_MultipleCalls(t *testing.T) {
	content := `<tool_call>
{"name": "tool_one", "arguments": {"a": 1}}
</tool_call>
<tool_call>
{"name": "tool_two", "arguments": {"b": "hello"}}
</tool_call>`

	calls, textBefore, ok := extractXMLToolCalls(content)
	require.True(t, ok)
	require.Len(t, calls, 2)
	assert.Empty(t, textBefore)
	assert.Equal(t, "tool_one", calls[0].Function.Name)
	assert.Equal(t, "call_0", calls[0].ID)
	assert.Equal(t, "tool_two", calls[1].Function.Name)
	assert.Equal(t, "call_1", calls[1].ID)
}

func TestExtractXMLToolCalls_NullArguments(t *testing.T) {
	content := `<tool_call>{"name": "ping", "arguments": null}</tool_call>`

	calls, _, ok := extractXMLToolCalls(content)
	require.True(t, ok)
	require.Len(t, calls, 1)
	assert.Equal(t, "{}", calls[0].Function.Arguments)
}

func TestExtractXMLToolCalls_MissingArguments(t *testing.T) {
	content := `<tool_call>{"name": "ping"}</tool_call>`

	calls, _, ok := extractXMLToolCalls(content)
	require.True(t, ok)
	require.Len(t, calls, 1)
	assert.Equal(t, "{}", calls[0].Function.Arguments)
}

func TestExtractXMLToolCalls_InvalidJSON(t *testing.T) {
	content := `<tool_call>not-valid-json</tool_call>`

	calls, _, ok := extractXMLToolCalls(content)
	assert.False(t, ok)
	assert.Empty(t, calls)
}

func TestExtractXMLToolCalls_MissingName(t *testing.T) {
	content := `<tool_call>{"arguments": {"x": 1}}</tool_call>`

	calls, _, ok := extractXMLToolCalls(content)
	assert.False(t, ok)
	assert.Empty(t, calls)
}

func TestExtractXMLToolCalls_InlineNoWhitespace(t *testing.T) {
	content := `<tool_call>{"name":"ls","arguments":{"path":"/tmp"}}</tool_call>`

	calls, _, ok := extractXMLToolCalls(content)
	require.True(t, ok)
	require.Len(t, calls, 1)
	assert.Equal(t, "ls", calls[0].Function.Name)
	assert.JSONEq(t, `{"path": "/tmp"}`, calls[0].Function.Arguments)
}

func TestExtractXMLToolCalls_TextAfterCallDiscarded(t *testing.T) {
	content := "Preamble\n<tool_call>\n{\"name\": \"run\", \"arguments\": {}}\n</tool_call>\nSome trailing text."

	calls, textBefore, ok := extractXMLToolCalls(content)
	require.True(t, ok)
	require.Len(t, calls, 1)
	assert.Equal(t, "Preamble", textBefore)
}
