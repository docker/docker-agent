package dialog

import (
	"github.com/docker/docker-agent/pkg/runtime"
)

// ToolRejectionDialogID is the unique identifier for the tool rejection reason dialog.
const ToolRejectionDialogID = "tool-rejection-reason"

// toolRejectionOptions defines the preset rejection reasons.
// These match the original presets from tool_confirmation.go.
var toolRejectionOptions = []MultiChoiceOption{
	{
		ID:    "bad_args",
		Label: "Bad arguments",
		Value: "The arguments provided are incorrect or invalid.",
	},
	{
		ID:    "wrong_tool",
		Label: "Wrong tool",
		Value: "This is the wrong tool for this task.",
	},
	{
		ID:    "unsafe",
		Label: "Unsafe",
		Value: "This action could be unsafe or destructive.",
	},
	{
		ID:    "clarify",
		Label: "Clarify first",
		Value: "Please clarify what you're trying to accomplish.",
	},
}

// NewToolRejectionReasonDialog creates a multi-choice dialog for selecting
// the reason for rejecting a tool call.
func NewToolRejectionReasonDialog() Dialog {
	return NewMultiChoiceDialog(MultiChoiceConfig{
		DialogID:          ToolRejectionDialogID,
		Title:             "Why reject this tool call?",
		Options:           toolRejectionOptions,
		AllowCustom:       true,
		AllowSecondary:    true,
		SecondaryLabel:    "Skip",
		PrimaryLabel:      "Reject",
		CustomPlaceholder: "Other reason...",
	})
}

// HandleToolRejectionResult processes the result from the tool rejection dialog
// and returns the appropriate RuntimeResumeMsg.
// Returns nil if the result was cancelled (user should stay in confirmation dialog).
func HandleToolRejectionResult(result MultiChoiceResult) *RuntimeResumeMsg {
	if result.IsCancelled {
		// User pressed Esc - don't send resume, let them stay in confirmation dialog
		return nil
	}

	// Build the reason string
	reason := result.Value
	if result.IsSkipped {
		reason = "" // No reason provided
	}

	return &RuntimeResumeMsg{
		Request: runtime.ResumeReject(reason),
	}
}
