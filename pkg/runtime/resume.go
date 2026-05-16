package runtime

import "github.com/docker/docker-agent/pkg/runtime/toolexec"

// ResumeType identifies the user's response to a confirmation request.
//
// The runtime emits a TOOL_PERMISSION_REQUEST event whenever a tool call
// requires user approval, then blocks until the embedder calls Resume(...)
// with one of the values below.
//
// ResumeType, ResumeRequest, and the ResumeType* constants are aliased
// from [toolexec] so the dispatcher and the runtime share one definition
// without circular imports.
type ResumeType = toolexec.ResumeType

const (
	// ResumeTypeApprove approves the single pending tool call.
	ResumeTypeApprove = toolexec.ResumeTypeApprove
	// ResumeTypeApproveSession approves the pending tool call and every
	// subsequent permission-gated call for the rest of the session.
	ResumeTypeApproveSession = toolexec.ResumeTypeApproveSession
	// ResumeTypeApproveTool approves the pending call and every future
	// call to the same tool name within the session.
	ResumeTypeApproveTool = toolexec.ResumeTypeApproveTool
	// ResumeTypeReject rejects the pending tool call.
	ResumeTypeReject = toolexec.ResumeTypeReject
)

// ResumeRequest carries the user's confirmation decision along with an optional
// reason (used when rejecting a tool call to help the model understand why).
// The struct fields live in [toolexec.ResumeRequest]; this alias is kept
// for readers who land here from the runtime API.
type ResumeRequest = toolexec.ResumeRequest

// ResumeApprove creates a ResumeRequest to approve a single tool call.
func ResumeApprove() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApprove}
}

// ResumeApproveSession creates a ResumeRequest to approve all tool calls for the session.
func ResumeApproveSession() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveSession}
}

// ResumeApproveTool creates a ResumeRequest to always approve a specific tool for the session.
func ResumeApproveTool(toolName string) ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveTool, ToolName: toolName}
}

// ResumeReject creates a ResumeRequest to reject a tool call with an optional reason.
func ResumeReject(reason string) ResumeRequest {
	return ResumeRequest{Type: ResumeTypeReject, Reason: reason}
}

// IsValidResumeType validates confirmation values coming from /resume.
//
// The runtime may be resumed by multiple entry points (API, CLI, TUI, tests).
// Even if upstream layers perform validation, the runtime must never assume
// the ResumeType is valid; accepting invalid values leads to confusing
// downstream behaviour where tool execution fails without a clear cause.
func IsValidResumeType(t ResumeType) bool {
	switch t {
	case ResumeTypeApprove,
		ResumeTypeApproveSession,
		ResumeTypeApproveTool,
		ResumeTypeReject:
		return true
	default:
		return false
	}
}

// ValidResumeTypes returns all allowed confirmation values, in declaration order.
func ValidResumeTypes() []ResumeType {
	return []ResumeType{
		ResumeTypeApprove,
		ResumeTypeApproveSession,
		ResumeTypeApproveTool,
		ResumeTypeReject,
	}
}
