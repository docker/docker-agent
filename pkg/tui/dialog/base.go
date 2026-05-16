package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// ConfirmKeyMap defines key bindings for confirmation dialogs (Yes/No).
type ConfirmKeyMap struct {
	Yes key.Binding
	No  key.Binding
}

// DefaultConfirmKeyMap returns the standard Yes/No key bindings.
func DefaultConfirmKeyMap() ConfirmKeyMap {
	return ConfirmKeyMap{
		Yes: key.NewBinding(
			key.WithKeys("y", "Y"),
			key.WithHelp("Y", "yes"),
		),
		No: key.NewBinding(
			key.WithKeys("n", "N"),
			key.WithHelp("N", "no"),
		),
	}
}

// BaseDialog provides common functionality for dialog implementations.
// It handles size management, position calculation, and common UI patterns.
type BaseDialog struct {
	width, height int
}

// SetSize updates the dialog dimensions.
func (b *BaseDialog) SetSize(width, height int) tea.Cmd {
	b.width = width
	b.height = height
	return nil
}

// Width returns the current width.
func (b *BaseDialog) Width() int {
	return b.width
}

// Height returns the current height.
func (b *BaseDialog) Height() int {
	return b.height
}

// ComputeDialogWidth calculates dialog width based on screen percentage with bounds.
func (b *BaseDialog) ComputeDialogWidth(percent, minWidth, maxWidth int) int {
	width := b.width * percent / 100
	if width < minWidth {
		width = max(20, min(b.width-4, minWidth))
	}
	if width > maxWidth {
		width = min(maxWidth, b.width-4)
	}
	return width
}

// ContentWidth calculates the inner content width given dialog width and padding.
func (b *BaseDialog) ContentWidth(dialogWidth, paddingX int) int {
	// Border takes one character on each side
	frameHorizontal := (paddingX * 2) + 2
	return max(10, dialogWidth-frameHorizontal)
}

// CenterDialog returns the (row, col) position to center a rendered dialog.
func (b *BaseDialog) CenterDialog(renderedDialog string) (row, col int) {
	dialogWidth := lipgloss.Width(renderedDialog)
	dialogHeight := lipgloss.Height(renderedDialog)
	return CenterPosition(b.width, b.height, dialogWidth, dialogHeight)
}

// ContentStartRow returns the absolute Y row where content begins inside a dialog.
// dialogRow is the top-left row of the dialog, and headerContent is the rendered
// header text above the target content area. The dialog frame (border + padding)
// is accounted for automatically using DialogStyle.
func ContentStartRow(dialogRow int, headerContent string) int {
	frameTop := styles.DialogStyle.GetBorderTopSize() + styles.DialogStyle.GetPaddingTop()
	return dialogRow + frameTop + lipgloss.Height(headerContent)
}

// ContentEndRow returns the absolute Y row of the last content line inside a dialog.
// dialogRow is the top-left row and dialogHeight is the total rendered height.
// The dialog frame (border + padding) is accounted for automatically using DialogStyle.
func ContentEndRow(dialogRow, dialogHeight int) int {
	frameBottom := styles.DialogStyle.GetBorderBottomSize() + styles.DialogStyle.GetPaddingBottom()
	return dialogRow + dialogHeight - 1 - frameBottom
}

// CloseWithElicitationResponse returns a command that closes the dialog and sends an elicitation response.
func CloseWithElicitationResponse(action tools.ElicitationAction, content map[string]any) tea.Cmd {
	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(messages.ElicitationResponseMsg{Action: action, Content: content}),
	)
}

// RenderTitle renders a dialog title with the given style and width.
func RenderTitle(title string, contentWidth int, style lipgloss.Style) string {
	return style.Width(contentWidth).Render(title)
}

// RenderSeparator renders a horizontal separator line.
func RenderSeparator(contentWidth int) string {
	separatorWidth := max(1, contentWidth)
	return styles.DialogSeparatorStyle.
		Align(lipgloss.Center).
		Width(contentWidth).
		Render(strings.Repeat("─", separatorWidth))
}

// RenderGroupSeparator renders a labelled section separator inside a list,
// like "── Custom themes ──────────────". It is used to visually divide
// groups of items in a picker list.
func RenderGroupSeparator(label string, contentWidth int) string {
	prefix := "── " + strings.TrimSpace(label) + " "
	dashes := max(0, contentWidth-lipgloss.Width(prefix)-2)
	return styles.MutedStyle.Render(prefix + strings.Repeat("─", dashes))
}

// RenderHelp renders help text at the bottom of a dialog in italic muted style.
func RenderHelp(text string, contentWidth int) string {
	return styles.DialogHelpStyle.Width(contentWidth).Align(lipgloss.Center).Render(text)
}

// RenderHelpKeys renders key bindings in the same style as the main TUI's status bar.
// Each binding is a pair of [key, description] strings.
func RenderHelpKeys(contentWidth int, bindings ...string) string {
	if len(bindings) == 0 || len(bindings)%2 != 0 {
		return ""
	}

	var parts []string
	for i := 0; i < len(bindings); i += 2 {
		keyPart := styles.HighlightWhiteStyle.Render(bindings[i])
		descPart := styles.SecondaryStyle.Render(bindings[i+1])
		parts = append(parts, keyPart+" "+descPart)
	}

	return styles.BaseStyle.Width(contentWidth).Align(lipgloss.Center).Render(strings.Join(parts, "  "))
}

// HandleQuit checks for ctrl+c and returns tea.Quit if matched.
func HandleQuit(msg tea.KeyPressMsg) tea.Cmd {
	if msg.String() == "ctrl+c" {
		return tea.Quit
	}
	return nil
}

// HandleConfirmKeys handles Yes/No key presses for confirmation dialogs.
// Returns the command to execute and whether a key was matched.
func HandleConfirmKeys(msg tea.KeyPressMsg, keyMap ConfirmKeyMap, onYes, onNo func() (layout.Model, tea.Cmd)) (layout.Model, tea.Cmd, bool) {
	switch {
	case key.Matches(msg, keyMap.Yes):
		model, cmd := onYes()
		return model, cmd, true
	case key.Matches(msg, keyMap.No):
		model, cmd := onNo()
		return model, cmd, true
	}
	return nil, nil, false
}

// Content helps build dialog content with consistent structure.
type Content struct {
	width int
	parts []string
}

// NewContent creates a new dialog content builder.
func NewContent(contentWidth int) *Content {
	return &Content{width: contentWidth}
}

// AddTitle adds a styled title to the dialog.
func (dc *Content) AddTitle(title string) *Content {
	dc.parts = append(dc.parts, RenderTitle(title, dc.width, styles.DialogTitleStyle))
	return dc
}

// AddSeparator adds a horizontal separator line.
func (dc *Content) AddSeparator() *Content {
	dc.parts = append(dc.parts, RenderSeparator(dc.width))
	return dc
}

// AddSpace adds an empty line for spacing.
func (dc *Content) AddSpace() *Content {
	dc.parts = append(dc.parts, "")
	return dc
}

// AddQuestion adds a styled question text.
func (dc *Content) AddQuestion(question string) *Content {
	dc.parts = append(dc.parts, styles.DialogQuestionStyle.Width(dc.width).Render(question))
	return dc
}

// AddContent adds raw content to the dialog.
func (dc *Content) AddContent(content string) *Content {
	dc.parts = append(dc.parts, content)
	return dc
}

// AddHelpKeys adds key binding help at the bottom.
func (dc *Content) AddHelpKeys(bindings ...string) *Content {
	dc.parts = append(dc.parts, RenderHelpKeys(dc.width, bindings...))
	return dc
}

// AddHelp adds help text at the bottom.
func (dc *Content) AddHelp(text string) *Content {
	dc.parts = append(dc.parts, RenderHelp(text, dc.width))
	return dc
}

// Build returns the final dialog content as a vertical join.
func (dc *Content) Build() string {
	return lipgloss.JoinVertical(lipgloss.Left, dc.parts...)
}
