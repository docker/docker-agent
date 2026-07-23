package runtime

import "github.com/docker/docker-agent/pkg/runtime/toolexec"

// ResumeType identifies the user's response to a confirmation request.
// Aliased from [toolexec] to keep dispatcher + runtime in sync without
// circular imports.
type ResumeType = toolexec.ResumeType

const (
	// ResumeTypeApprove approves the single pending tool call.
	ResumeTypeApprove = toolexec.ResumeTypeApprove
	// ResumeTypeApproveBalanced approves + flips session to
	// [session.SafetyPolicyBalanced].
	ResumeTypeApproveBalanced = toolexec.ResumeTypeApproveBalanced
	// ResumeTypeApproveAutonomous approves + flips session to
	// [session.SafetyPolicyAutonomous].
	ResumeTypeApproveAutonomous = toolexec.ResumeTypeApproveAutonomous
	// ResumeTypeApproveTool approves + appends the tool to the
	// session's Allow list.
	ResumeTypeApproveTool = toolexec.ResumeTypeApproveTool
	// ResumeTypeReject rejects the pending tool call.
	ResumeTypeReject = toolexec.ResumeTypeReject
)

// ResumeRequest carries the user's confirmation decision plus an
// optional reason (used with Reject to help the model understand why).
type ResumeRequest = toolexec.ResumeRequest

func ResumeApprove() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApprove}
}

func ResumeApproveBalanced() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveBalanced}
}

func ResumeApproveAutonomous() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveAutonomous}
}

func ResumeApproveTool(toolName string) ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveTool, ToolName: toolName}
}

func ResumeReject(reason string) ResumeRequest {
	return ResumeRequest{Type: ResumeTypeReject, Reason: reason}
}

// IsValidResumeType rejects unknown ResumeType values arriving from
// external callers (API, CLI, TUI, tests). Unvalidated values lead to
// tool execution failing without a clear cause.
func IsValidResumeType(t ResumeType) bool {
	switch t {
	case ResumeTypeApprove,
		ResumeTypeApproveBalanced,
		ResumeTypeApproveAutonomous,
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
		ResumeTypeApproveBalanced,
		ResumeTypeApproveAutonomous,
		ResumeTypeApproveTool,
		ResumeTypeReject,
	}
}
