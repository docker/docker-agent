package runtime

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/docker/docker-agent/pkg/tools"
)

// xmlToolCallRe matches <tool_call>…</tool_call> blocks emitted by some
// llama.cpp-backed models (e.g. Qwen3-coder, Hermes) instead of the OpenAI
// function-calling API.
var xmlToolCallRe = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

// xmlToolCallPayload is the JSON structure emitted inside a <tool_call> block.
type xmlToolCallPayload struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// extractXMLToolCalls scans content for <tool_call>…</tool_call> blocks and
// returns parsed tool calls plus any text preceding the first block. Trailing
// text after the last block is discarded. ok is false when no valid blocks
// are found.
func extractXMLToolCalls(content string) (calls []tools.ToolCall, textBefore string, ok bool) {
	locs := xmlToolCallRe.FindAllStringIndex(content, -1)
	if len(locs) == 0 {
		return nil, "", false
	}

	textBefore = strings.TrimRight(content[:locs[0][0]], "\n")

	for i, loc := range locs {
		sub := xmlToolCallRe.FindStringSubmatch(content[loc[0]:loc[1]])
		if len(sub) < 2 {
			continue
		}

		var payload xmlToolCallPayload
		if err := json.Unmarshal([]byte(sub[1]), &payload); err != nil || payload.Name == "" {
			continue
		}

		// absent or null arguments → "{}"
		args := "{}"
		if len(payload.Arguments) > 0 && string(payload.Arguments) != "null" {
			args = string(payload.Arguments)
		}

		calls = append(calls, tools.ToolCall{
			ID:   fmt.Sprintf("call_%d", i),
			Type: "function",
			Function: tools.FunctionCall{
				Name:      payload.Name,
				Arguments: args,
			},
		})
	}

	return calls, textBefore, len(calls) > 0
}
