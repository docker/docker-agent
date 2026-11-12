package dialog

import "github.com/docker/docker-agent/pkg/tui/dialog/toolconfirmation"

const ToolRejectionDialogID = toolconfirmation.ToolRejectionDialogID

func NewToolRejectionReasonDialog() Dialog {
	return toolconfirmation.NewToolRejectionReasonDialog()
}

func HandleToolRejectionResult(result MultiChoiceResult) *RuntimeResumeMsg {
	return toolconfirmation.HandleToolRejectionResult(result)
}
