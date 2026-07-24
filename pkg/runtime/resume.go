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
	// ResumeTypeApproveBalanced approves the pending call and flips
	// the session to [session.SafetyPolicyBalanced] so subsequent
	// classifier-safe calls auto-approve.
	ResumeTypeApproveBalanced = toolexec.ResumeTypeApproveBalanced
	// ResumeTypeApproveAutonomous approves the pending call and flips
	// the session to [session.SafetyPolicyAutonomous] so every
	// subsequent call auto-approves (custom deny/ask rules still win).
	ResumeTypeApproveAutonomous = toolexec.ResumeTypeApproveAutonomous
	// ResumeTypeApproveTool approves the pending call and every future
	// call to the same tool name within the session.
	ResumeTypeApproveTool = toolexec.ResumeTypeApproveTool
	// ResumeTypeReject rejects the pending tool call.
	ResumeTypeReject = toolexec.ResumeTypeReject
)

// Legacy resume verbs, accepted and normalized for older callers.
//
//nolint:staticcheck // deliberate re-export of the deprecated aliases
const (
	// Deprecated: use [ResumeTypeApproveAutonomous].
	ResumeTypeApproveSession = toolexec.ResumeTypeApproveSession
	// Deprecated: use [ResumeTypeApproveBalanced].
	ResumeTypeApproveSafe = toolexec.ResumeTypeApproveSafe
	// Deprecated: use [ResumeTypeApproveBalanced].
	ResumeTypeApproveSafer = toolexec.ResumeTypeApproveSafer
)

// NormalizeResumeType maps legacy resume verbs onto the current set.
// See [toolexec.NormalizeResumeType].
func NormalizeResumeType(t ResumeType) ResumeType {
	return toolexec.NormalizeResumeType(t)
}

// ResumeRequest carries the user's confirmation decision along with an optional
// reason (used when rejecting a tool call to help the model understand why).
// The struct fields live in [toolexec.ResumeRequest]; this alias is kept
// for readers who land here from the runtime API.
type ResumeRequest = toolexec.ResumeRequest

// ResumeApprove creates a ResumeRequest to approve a single tool call.
func ResumeApprove() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApprove}
}

// ResumeApproveBalanced creates a ResumeRequest that approves the
// pending call and flips the session to SafetyPolicyBalanced.
func ResumeApproveBalanced() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveBalanced}
}

// ResumeApproveAutonomous creates a ResumeRequest that approves the
// pending call and flips the session to SafetyPolicyAutonomous.
func ResumeApproveAutonomous() ResumeRequest {
	return ResumeRequest{Type: ResumeTypeApproveAutonomous}
}

// ResumeApproveSession creates a ResumeRequest to approve all tool calls
// for the session.
//
// Deprecated: use [ResumeApproveAutonomous].
func ResumeApproveSession() ResumeRequest {
	return ResumeApproveAutonomous()
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
// Legacy verbs (approve-session, approve-safe, approve-safer) stay
// accepted; [NormalizeResumeType] maps them onto the current set.
//
// The runtime may be resumed by multiple entry points (API, CLI, TUI, tests).
// Even if upstream layers perform validation, the runtime must never assume
// the ResumeType is valid; accepting invalid values leads to confusing
// downstream behaviour where tool execution fails without a clear cause.
func IsValidResumeType(t ResumeType) bool {
	switch NormalizeResumeType(t) {
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

// ValidResumeTypes returns the current (non-legacy) confirmation values,
// in declaration order.
func ValidResumeTypes() []ResumeType {
	return []ResumeType{
		ResumeTypeApprove,
		ResumeTypeApproveBalanced,
		ResumeTypeApproveAutonomous,
		ResumeTypeApproveTool,
		ResumeTypeReject,
	}
}
