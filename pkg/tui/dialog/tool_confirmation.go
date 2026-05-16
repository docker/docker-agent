package dialog

import (
	"encoding/json"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// Layout constants for tool confirmation dialog.
const (
	toolConfirmDialogWidthPercent  = 70 // Dialog width as percentage of screen
	toolConfirmDialogHeightPercent = 80 // Max dialog height as percentage of screen
	toolConfirmMinScrollHeight     = 5  // Minimum height for the scroll view
	toolConfirmEmptyLinesBefore    = 2  // Empty lines before question
	toolConfirmEmptyLinesAfter     = 1  // Empty lines after question
)

type (
	RuntimeResumeMsg struct {
		Request runtime.ResumeRequest
	}
)

// ToolConfirmationResponse represents the user's response to tool confirmation
type ToolConfirmationResponse struct {
	Response string // "approve", "reject", or "approve-session"
}

type toolConfirmationDialog struct {
	BaseDialog

	msg               *runtime.ToolCallConfirmationEvent
	keyMap            toolConfirmationKeyMap
	sessionState      *service.SessionState
	scrollView        messages.Model
	permissionPattern string // cached permission pattern for this tool call
}

// dialogDimensions returns computed dialog width and content width.
func (d *toolConfirmationDialog) dialogDimensions() (dialogWidth, contentWidth int) {
	dialogWidth = d.Width() * toolConfirmDialogWidthPercent / 100
	contentWidth = dialogWidth - styles.DialogStyle.GetHorizontalFrameSize()
	return dialogWidth, contentWidth
}

// SetSize implements [Dialog].
func (d *toolConfirmationDialog) SetSize(width, height int) tea.Cmd {
	d.BaseDialog.SetSize(width, height)

	// Calculate dialog dimensions using helper
	_, contentWidth := d.dialogDimensions()
	maxDialogHeight := height * toolConfirmDialogHeightPercent / 100

	// Measure fixed UI elements using the same rendering as View()
	titleStyle := styles.DialogTitleStyle.Width(contentWidth)
	title := titleStyle.Render("Tool Confirmation")
	titleHeight := lipgloss.Height(title)

	separator := d.renderSeparator(contentWidth)
	separatorHeight := lipgloss.Height(separator)

	question := styles.DialogQuestionStyle.Width(contentWidth).Render("Do you want to allow this tool call?")
	questionHeight := lipgloss.Height(question)

	options := RenderHelpKeys(contentWidth, "Y", "yes", "N", "no", "T", d.alwaysAllowHelpText(), "A", "all tools")
	optionsHeight := lipgloss.Height(options)

	// Calculate available height for scroll view
	frameHeight := styles.DialogStyle.GetVerticalFrameSize()
	fixedContentHeight := titleHeight + separatorHeight + toolConfirmEmptyLinesBefore + questionHeight + toolConfirmEmptyLinesAfter + optionsHeight
	availableHeight := max(maxDialogHeight-frameHeight-fixedContentHeight, toolConfirmMinScrollHeight)
	d.scrollView.SetSize(contentWidth, availableHeight)

	return nil
}

// renderSeparator renders the separator line consistently.
func (d *toolConfirmationDialog) renderSeparator(contentWidth int) string {
	return RenderSeparator(contentWidth)
}

// alwaysAllowHelpText returns a descriptive help text for the "always allow" option.
// For shell commands, it shows the command pattern (e.g., "always allow ls*").
// For other tools, it shows "always allow <toolname>".
func (d *toolConfirmationDialog) alwaysAllowHelpText() string {
	pattern := d.permissionPattern
	toolName := d.msg.ToolCall.Function.Name

	// For shell with a command pattern, show a more descriptive label
	if toolName == "shell" {
		if _, cmdPattern, ok := strings.Cut(pattern, ":cmd="); ok {
			return "always allow " + cmdPattern
		}
	}

	return "always allow " + toolName
}

// toolConfirmationKeyMap defines key bindings for tool confirmation dialog
type toolConfirmationKeyMap struct {
	Yes      key.Binding
	No       key.Binding
	All      key.Binding
	ThisTool key.Binding
}

// defaultToolConfirmationKeyMap returns default key bindings
func defaultToolConfirmationKeyMap() toolConfirmationKeyMap {
	return toolConfirmationKeyMap{
		Yes: key.NewBinding(
			key.WithKeys("y", "Y"),
			key.WithHelp("Y", "approve"),
		),
		No: key.NewBinding(
			key.WithKeys("n", "N"),
			key.WithHelp("N", "reject"),
		),
		All: key.NewBinding(
			key.WithKeys("a", "A"),
			key.WithHelp("A", "approve all"),
		),
		ThisTool: key.NewBinding(
			key.WithKeys("t", "T"),
			key.WithHelp("T", "always allow this tool"),
		),
	}
}

// buildPermissionPattern creates a permission pattern for the tool call.
// For shell commands, it extracts the first word of the command and creates
// a pattern like "shell:cmd=ls*" to match all invocations of that command.
// For other tools, it returns just the tool name.
func buildPermissionPattern(toolCall tools.ToolCall) string {
	toolName := toolCall.Function.Name

	// For shell tool, extract the command and create a pattern
	if toolName == "shell" {
		var args struct {
			Cmd string `json:"cmd"`
		}
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err == nil {
			// Extract the first word (the command) from the full command string
			// e.g., "ls -la /tmp" -> "ls"
			// strings.Fields handles all whitespace (space, tab, newline)
			if fields := strings.Fields(args.Cmd); len(fields) > 0 {
				// Create pattern: shell:cmd=<command>*
				// The trailing * allows matching with any arguments
				return toolName + ":cmd=" + fields[0] + "*"
			}
		}
	}

	// For other tools, just return the tool name
	return toolName
}

// NewToolConfirmationDialog creates a new tool confirmation dialog
func NewToolConfirmationDialog(msg *runtime.ToolCallConfirmationEvent, sessionState *service.SessionState) Dialog {
	// Create scrollable view with minimal initial size (will be updated in SetSize)
	scrollView := messages.NewScrollableView(1, 1, sessionState)

	// Add the tool call message to the view
	scrollView.AddOrUpdateToolCall(
		"", // agentName - empty for dialog context
		msg.ToolCall,
		msg.ToolDefinition,
		types.ToolStatusConfirmation,
	)

	// Build and cache the permission pattern for display and use
	pattern := buildPermissionPattern(msg.ToolCall)

	return &toolConfirmationDialog{
		msg:               msg,
		sessionState:      sessionState,
		keyMap:            defaultToolConfirmationKeyMap(),
		scrollView:        scrollView,
		permissionPattern: pattern,
	}
}

// Init initializes the tool confirmation dialog
func (d *toolConfirmationDialog) Init() tea.Cmd {
	return d.scrollView.Init()
}

// executeAction dispatches a confirmation action by key ("Y", "N", "T", "A").
func (d *toolConfirmationDialog) executeAction(action string) (layout.Model, tea.Cmd) {
	switch action {
	case "Y":
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: runtime.ResumeApprove()}),
		)
	case "N":
		return d, core.CmdHandler(OpenDialogMsg{
			Model: NewToolRejectionReasonDialog(),
		})
	case "T":
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: runtime.ResumeApproveTool(d.permissionPattern)}),
		)
	case "A":
		d.sessionState.SetYoloMode(true)
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: runtime.ResumeApproveSession()}),
		)
	}
	return d, nil
}

// Update handles messages for the tool confirmation dialog
func (d *toolConfirmationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return d.handleMouseClick(msg)
		}
		return d, nil

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}

		switch {
		case key.Matches(msg, d.keyMap.Yes):
			return d.executeAction("Y")
		case key.Matches(msg, d.keyMap.No):
			return d.executeAction("N")
		case key.Matches(msg, d.keyMap.All):
			return d.executeAction("A")
		case key.Matches(msg, d.keyMap.ThisTool):
			return d.executeAction("T")
		}

		// Forward scrolling keys to the scroll view
		if _, isScrollKey := core.GetScrollDirection(msg); isScrollKey {
			updatedScrollView, cmd := d.scrollView.Update(msg)
			d.scrollView = updatedScrollView.(messages.Model)
			return d, cmd
		}

	case tuimessages.WheelCoalescedMsg:
		updatedScrollView, cmd := d.scrollView.Update(msg)
		d.scrollView = updatedScrollView.(messages.Model)
		return d, cmd
	}

	return d, nil
}

// handleMouseClick handles mouse clicks on the action buttons (Y/N/T/A).
func (d *toolConfirmationDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	dialogRow, dialogCol := d.Position()
	renderedDialog := d.View()
	dialogHeight := lipgloss.Height(renderedDialog)

	// The options line is the last content line inside the dialog.
	if msg.Y != ContentEndRow(dialogRow, dialogHeight) {
		return d, nil
	}

	// Render the help keys and strip ANSI to get plain text for hit-testing.
	_, contentWidth := d.dialogDimensions()
	options := RenderHelpKeys(contentWidth, "Y", "yes", "N", "no", "T", d.alwaysAllowHelpText(), "A", "all tools")
	optionsPlain := ansi.Strip(options)

	// Content starts after left border + padding.
	frameLeft := styles.DialogStyle.GetBorderLeftSize() + styles.DialogStyle.GetPaddingLeft()

	// The help text is center-aligned within contentWidth.
	plainLen := len(optionsPlain)
	leadingSpaces := max(0, (contentWidth-plainLen)/2)
	relX := msg.X - dialogCol - frameLeft - leadingSpaces
	if relX < 0 || relX >= plainLen {
		return d, nil
	}

	// Walk backward from the click position to find the nearest action key.
	// The plain text looks like: "Y yes  N no  T always allow...  A all tools"
	// Each region starts with its uppercase action key.
	actionKeys := "YNTA"
	for i := relX; i >= 0; i-- {
		if strings.ContainsRune(actionKeys, rune(optionsPlain[i])) {
			return d.executeAction(string(optionsPlain[i]))
		}
	}

	return d, nil
}

// View renders the tool confirmation dialog
func (d *toolConfirmationDialog) View() string {
	dialogWidth, contentWidth := d.dialogDimensions()

	dialogStyle := styles.DialogStyle.Width(dialogWidth)

	titleStyle := styles.DialogTitleStyle.Width(contentWidth)
	title := titleStyle.Render("Tool Confirmation")

	// Separator
	separator := d.renderSeparator(contentWidth)

	// Get scrollable tool call view
	argumentsSection := d.scrollView.View()

	// Combine all parts with proper spacing
	parts := []string{title, separator}

	if argumentsSection != "" {
		parts = append(parts, "", argumentsSection)
	}

	// Confirmation prompt
	question := styles.DialogQuestionStyle.Width(contentWidth).Render("Do you want to allow this tool call?")
	options := RenderHelpKeys(contentWidth, "Y", "yes", "N", "no", "T", d.alwaysAllowHelpText(), "A", "all tools")

	parts = append(parts, "", question, "", options)

	content := lipgloss.JoinVertical(lipgloss.Left, parts...)

	return dialogStyle.Render(content)
}

// Position calculates the position to center the dialog
func (d *toolConfirmationDialog) Position() (row, col int) {
	dialogWidth, _ := d.dialogDimensions()
	renderedDialog := d.View()
	dialogHeight := lipgloss.Height(renderedDialog)
	return CenterPosition(d.Width(), d.Height(), dialogWidth, dialogHeight)
}
