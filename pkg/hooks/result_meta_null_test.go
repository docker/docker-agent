package hooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStdoutJSONMetadataPresence(t *testing.T) {
	t.Parallel()
	for _, strict := range []bool{false, true} {
		for _, tt := range []struct {
			name, output, expected string
			present                bool
		}{
			{"omitted", `{"hook_specific_output":{}}`, "", false},
			{"null", `{"hook_specific_output":{"updated_tool_result_meta":null}}`, "null", true},
			{"object", `{"hook_specific_output":{"updated_tool_result_meta":{"safe":true}}}`, `{"safe":true}`, true},
		} {
			t.Run(tt.name+map[bool]string{true: "/strict", false: "/lenient"}[strict], func(t *testing.T) {
				t.Parallel()
				output, err := parseStdoutJSON(tt.output, strict)
				require.NoError(t, err)
				meta := output.HookSpecificOutput.UpdatedToolResultMeta
				assert.Equal(t, tt.present, meta != nil)
				assert.Equal(t, tt.expected, string(meta))
				input := Input{ToolResultMeta: map[string]any{"secret": "original"}}
				rewritten, changed := rewrittenInput(input, EventToolResponseTransform, output.HookSpecificOutput)
				assert.Equal(t, tt.present, changed)
				result := aggregate([]hookResult{{HandlerResult: HandlerResult{Output: output}}}, EventToolResponseTransform)
				assert.Equal(t, meta, result.UpdatedToolResultMeta)
				if tt.present {
					assert.Equal(t, meta, rewritten.ToolResultMeta)
				} else {
					assert.Equal(t, input.ToolResultMeta, rewritten.ToolResultMeta)
				}
			})
		}
	}
}
