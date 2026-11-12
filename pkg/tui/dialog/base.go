package dialog

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog/common"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type (
	BaseDialog    = common.BaseDialog
	ConfirmKeyMap = common.ConfirmKeyMap
	Content       = common.Content
)

func DefaultConfirmKeyMap() ConfirmKeyMap {
	return common.DefaultConfirmKeyMap()
}

func CenterPosition(screenWidth, screenHeight, dialogWidth, dialogHeight int) (row, col int) {
	return common.CenterPosition(screenWidth, screenHeight, dialogWidth, dialogHeight)
}

func ContentStartRow(dialogRow int, headerContent string) int {
	return common.ContentStartRow(dialogRow, headerContent)
}

func ContentEndRow(dialogRow, dialogHeight int) int {
	return common.ContentEndRow(dialogRow, dialogHeight)
}

// CloseWithElicitationResponse returns a command that closes the dialog and sends an elicitation response.
func CloseWithElicitationResponse(action tools.ElicitationAction, content map[string]any, elicitationID string) tea.Cmd {
	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{ElicitationID: elicitationID}),
		core.CmdHandler(messages.ElicitationResponseMsg{Action: action, Content: content, ElicitationID: elicitationID}),
	)
}

func RenderTitle(title string, contentWidth int, style lipgloss.Style) string {
	return common.RenderTitle(title, contentWidth, style)
}

func RenderSeparator(contentWidth int) string {
	return common.RenderSeparator(contentWidth)
}

func RenderGroupSeparator(label string, contentWidth int) string {
	return common.RenderGroupSeparator(label, contentWidth)
}

func RenderHelp(text string, contentWidth int) string {
	return common.RenderHelp(text, contentWidth)
}

func RenderHelpKeys(contentWidth int, bindings ...string) string {
	return common.RenderHelpKeys(contentWidth, bindings...)
}

func HelpKeysWidth(bindings ...string) int {
	return common.HelpKeysWidth(bindings...)
}

func HandleQuit(msg tea.KeyPressMsg) tea.Cmd {
	return common.HandleQuit(msg)
}

func HandleConfirmKeys(msg tea.KeyPressMsg, keyMap ConfirmKeyMap, onYes, onNo func() (layout.Model, tea.Cmd)) (layout.Model, tea.Cmd, bool) {
	return common.HandleConfirmKeys(msg, keyMap, onYes, onNo)
}

func NewContent(contentWidth int) *Content {
	return common.NewContent(contentWidth)
}
