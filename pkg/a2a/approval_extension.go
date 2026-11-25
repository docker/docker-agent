package a2a

import (
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
)

// toolApprovalExtensionURI identifies the tool-approval A2A extension: the
// contract a client follows to answer a TASK_STATE_INPUT_REQUIRED tool call.
const toolApprovalExtensionURI = "https://docker.github.io/docker-agent/a2a/extensions/tool-approval/v1"

var (
	metaKeyType        = adka2a.ToA2AMetaKey("type")
	metaKeyLongRunning = adka2a.ToA2AMetaKey("is_long_running")
)

func toolApprovalExtension() *a2a.AgentExtension {
	return &a2a.AgentExtension{
		URI:         toolApprovalExtensionURI,
		Description: "Approve or reject tool calls of tasks paused in TASK_STATE_INPUT_REQUIRED.",
		Required:    false,
	}
}

// approvalResponseSchema is the JSON Schema of the function response a client
// sends back, readable by a UI and by a model. A2A metadata only accepts
// []any slices.
func approvalResponseSchema() map[string]any {
	enum := make([]any, len(allowedDecisions))
	for i, d := range allowedDecisions {
		enum[i] = d
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{
				"type": "string",
				"enum": enum,
				"description": "approve: run this call; approve-tool: run it and allow this tool for the rest " +
					"of the conversation; reject: skip it",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Optional explanation passed to the model on reject",
			},
		},
		"required": []any{"decision"},
	}
}

// annotateApprovalParts adds the extension metadata to the pending
// function-call parts of an INPUT_REQUIRED status update.
func annotateApprovalParts(event a2a.Event) {
	ev, ok := event.(*a2a.TaskStatusUpdateEvent)
	if !ok || ev.Status.State != a2a.TaskStateInputRequired || ev.Status.Message == nil {
		return
	}
	for _, part := range ev.Status.Message.Parts {
		data, ok := part.Data().(map[string]any)
		if !ok || part.Meta()[metaKeyType] != "function_call" || part.Meta()[metaKeyLongRunning] != true {
			continue
		}
		name, _ := data["name"].(string)
		part.SetMeta(toolApprovalExtensionURI, map[string]any{
			"prompt":         fmt.Sprintf("Tool %q requires approval", name),
			"responseSchema": approvalResponseSchema(),
		})
	}
}
