package toolexec

import (
	"encoding/json"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

// NewHooksInput builds a [hooks.Input] from the common tool-call fields.
// [hooks.Executor.Dispatch] auto-fills Cwd from the executor's working
// directory, so callers don't set it here.
func NewHooksInput(sess *session.Session, toolCall tools.ToolCall) *hooks.Input {
	return &hooks.Input{
		SessionID: sess.ID,
		ToolName:  toolCall.Function.Name,
		ToolUseID: toolCall.ID,
		ToolInput: ParseToolInput(toolCall.Function.Arguments),
	}
}

// NewPostToolHooksInput builds a [hooks.Input] for the post-tool-use event.
// It enriches the common fields built by [NewHooksInput] with the tool
// result so post-tool-use hooks can inspect the response and error flag.
func NewPostToolHooksInput(sess *session.Session, toolCall tools.ToolCall, res *tools.ToolCallResult) *hooks.Input {
	input := NewHooksInput(sess, toolCall)
	if res != nil {
		input.ToolResponse = res.Output
		input.ToolError = res.IsError
	}
	return input
}

// ParseToolInput parses a tool-call arguments JSON string into a map.
// Invalid or empty input yields a nil map; callers that need to distinguish
// "no arguments" from "invalid arguments" must inspect the input themselves.
func ParseToolInput(arguments string) map[string]any {
	var result map[string]any
	if err := json.Unmarshal([]byte(arguments), &result); err != nil {
		return nil
	}
	return result
}
