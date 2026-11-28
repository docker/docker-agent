package hooks

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
)

// TestToolDocumentsJSONRoundTripDistinguishesNilFromEmpty pins the wire
// contract of updated_tool_documents: nil means "leave the attachments
// unchanged" and is omitted from JSON, while an empty non-nil list means
// "remove every attachment" and must survive both marshaling (a Go hook
// writing its output) and parsing (a command hook reading its input back).
func TestToolDocumentsJSONRoundTripDistinguishesNilFromEmpty(t *testing.T) {
	t.Parallel()

	// nil: the field is omitted entirely, so the runtime keeps the current
	// attachment list.
	encoded, err := json.Marshal(&Output{HookSpecificOutput: &HookSpecificOutput{}})
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "updated_tool_documents")

	// Empty but non-nil: a literal empty array on the wire, not an omitted
	// field. omitempty would have dropped it here and turned the explicit
	// "remove all attachments" rewrite into a no-op.
	encoded, err = json.Marshal(&Output{HookSpecificOutput: &HookSpecificOutput{
		UpdatedToolDocuments: []tools.DocumentContent{},
	}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"hook_specific_output":{"updated_tool_documents":[]}}`, string(encoded))

	// Round-trip: an omitted field decodes to nil (no rewrite)...
	var omitted Output
	require.NoError(t, json.Unmarshal([]byte(`{"hook_specific_output":{}}`), &omitted))
	require.NotNil(t, omitted.HookSpecificOutput)
	assert.Nil(t, omitted.HookSpecificOutput.UpdatedToolDocuments)

	// ...while a literal [] decodes to a non-nil empty slice.
	cleared, err := parseStdoutJSON(`{"hook_specific_output":{"updated_tool_documents":[]}}`, false)
	require.NoError(t, err)
	require.NotNil(t, cleared)
	require.NotNil(t, cleared.HookSpecificOutput)
	assert.NotNil(t, cleared.HookSpecificOutput.UpdatedToolDocuments)
	assert.Empty(t, cleared.HookSpecificOutput.UpdatedToolDocuments)

	// The empty list participates in the sequential pipeline as a rewrite:
	// the rewritten input drops the attachments and aggregate keeps the
	// empty slice instead of falling back to "no rewrite".
	input := Input{ToolDocuments: []tools.DocumentContent{{Name: "notes.md", Text: "hello"}}}
	rewritten, changed := rewrittenInput(input, EventToolResponseTransform, cleared.HookSpecificOutput)
	assert.True(t, changed)
	assert.NotNil(t, rewritten.ToolDocuments)
	assert.Empty(t, rewritten.ToolDocuments)

	result := aggregate([]hookResult{{HandlerResult: HandlerResult{Output: cleared}}}, EventToolResponseTransform)
	assert.NotNil(t, result.UpdatedToolDocuments, "an explicit clear must not be conflated with nil")
	assert.Empty(t, result.UpdatedToolDocuments)
}
