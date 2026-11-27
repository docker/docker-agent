package a2a

import (
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"

	"github.com/docker/docker-agent/pkg/permissions"
)

// toolApprovalExtensionURI identifies the tool-approval A2A extension: the
// contract a client follows to answer a TASK_STATE_INPUT_REQUIRED tool call.
const toolApprovalExtensionURI = "https://docker.github.io/docker-agent/features/a2a-extensions/tool-approval/v1"

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
func approvalResponseSchema(grant string) map[string]any {
	decisions := allowedDecisions(grant)
	enum := make([]any, len(decisions))
	for i, d := range decisions {
		enum[i] = d
	}
	description := "approve: run this call only; reject: skip it"
	if grant != "" {
		description = fmt.Sprintf("approve: run this call only; approve-in-context: run it and allow later calls "+
			"matching %q in this context (contextId); reject: skip it", grant)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{
				"type":        "string",
				"enum":        enum,
				"description": description,
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
// function-call parts of an INPUT_REQUIRED status update. The grant comes from
// the parked run, so what is announced is exactly what approve-in-context stores.
func annotateApprovalParts(event a2a.Event, approvals *approvalRegistry) {
	ev, ok := event.(*a2a.TaskStatusUpdateEvent)
	if !ok || ev.Status.State != a2a.TaskStateInputRequired || ev.Status.Message == nil {
		return
	}
	run, ok := approvals.pending(ev.TaskID)
	if !ok {
		return
	}
	for _, part := range ev.Status.Message.Parts {
		data, ok := part.Data().(map[string]any)
		if !ok || part.Meta()[metaKeyType] != "function_call" || part.Meta()[metaKeyLongRunning] != true {
			continue
		}
		if id, _ := data["id"].(string); id != run.callID {
			continue
		}
		name, _ := data["name"].(string)
		meta := map[string]any{
			"prompt":         fmt.Sprintf("Tool %q requires approval", name),
			"responseSchema": approvalResponseSchema(run.grant),
		}
		if run.rawArgs != "" {
			meta["rawArguments"] = run.rawArgs
		}
		if run.grant != "" {
			meta["grant"] = run.grant
			meta["grantLabel"] = "allow " + permissions.PatternLabel(run.grant) + " in this context"
		}
		part.SetMeta(toolApprovalExtensionURI, meta)
	}
}
