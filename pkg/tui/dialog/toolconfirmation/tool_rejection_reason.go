package toolconfirmation

import (
	"github.com/docker/docker-agent/pkg/tui/components/toolconfirm"
	"github.com/docker/docker-agent/pkg/tui/dialog/common"
)

// ToolRejectionDialogID is the unique identifier for the tool rejection reason dialog.
const ToolRejectionDialogID = "tool-rejection-reason"

// toolRejectionOptions adapts the shared preset rejection reasons to the
// multi-choice dialog options.
func toolRejectionOptions() []common.MultiChoiceOption {
	reasons := toolconfirm.RejectionReasons()
	options := make([]common.MultiChoiceOption, 0, len(reasons))
	for _, r := range reasons {
		options = append(options, common.MultiChoiceOption{ID: r.ID, Label: r.Label, Value: r.Value})
	}
	return options
}

// NewToolRejectionReasonDialog creates a multi-choice dialog for selecting
// the reason for rejecting a tool call.
func NewToolRejectionReasonDialog() common.Dialog {
	return common.NewMultiChoiceDialog(common.MultiChoiceConfig{
		DialogID:          ToolRejectionDialogID,
		Title:             "Why reject this tool call?",
		Options:           toolRejectionOptions(),
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
func HandleToolRejectionResult(result common.MultiChoiceResult) *RuntimeResumeMsg {
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
		Request: toolconfirm.Reject.Resume("", reason),
	}
}
