package dialog

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/toolconfirm"
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

// ConfirmationSessionState is the session-state surface the confirmation
// dialog needs: the message list's surface for rendering the tool call, plus
// the session-wide approval flip for the "all tools" decision.
type ConfirmationSessionState interface {
	messages.SessionState
	SetYoloMode(yoloMode bool)
}

var (
	_ ConfirmationSessionState = (*service.SessionState)(nil)
	_ ConfirmationSessionState = (*service.EmbeddedSessionState)(nil)
)

type toolConfirmationDialog struct {
	BaseDialog

	msg               *runtime.ToolCallConfirmationEvent
	keyMap            toolconfirm.KeyMap
	sessionState      ConfirmationSessionState
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
	title := titleStyle.Render(toolconfirm.Title)
	titleHeight := lipgloss.Height(title)

	separator := d.renderSeparator(contentWidth)
	separatorHeight := lipgloss.Height(separator)

	question := styles.DialogQuestionStyle.Width(contentWidth).Render(toolconfirm.Question)
	questionHeight := lipgloss.Height(question)

	options := d.renderOptions(contentWidth)
	optionsHeight := lipgloss.Height(options)

	// The safety-warning + metadata sections each contribute their own
	// height plus a leading blank line (matching how View() spaces them).
	var safetyHeight int
	if warning := d.renderSafetyWarning(contentWidth); warning != "" {
		safetyHeight = lipgloss.Height(warning) + 1
	}
	var metadataHeight int
	if metadata := d.renderMetadata(contentWidth); metadata != "" {
		metadataHeight = lipgloss.Height(metadata) + 1
	}

	// Calculate available height for scroll view
	frameHeight := styles.DialogStyle.GetVerticalFrameSize()
	fixedContentHeight := titleHeight + separatorHeight + toolConfirmEmptyLinesBefore + questionHeight + toolConfirmEmptyLinesAfter + optionsHeight + safetyHeight + metadataHeight
	availableHeight := max(maxDialogHeight-frameHeight-fixedContentHeight, toolConfirmMinScrollHeight)
	d.scrollView.SetSize(contentWidth, availableHeight)

	return nil
}

// renderSeparator renders the separator line consistently.
func (d *toolConfirmationDialog) renderSeparator(contentWidth int) string {
	return RenderSeparator(contentWidth)
}

// renderOptions renders the Y/N/T/A decision row.
func (d *toolConfirmationDialog) renderOptions(contentWidth int) string {
	return RenderHelpKeys(contentWidth, toolconfirm.OptionsHelp(d.permissionPattern)...)
}

// safetyConventionKeys are the metadata keys the safer_shell builtin
// uses to surface its verdict to the UI when paired with a
// `blast_radius` key. The renderer composes a user-facing warning
// from these instead of showing raw key/value pairs (avoid leaking
// implementation details into the prompt).
//
// The convention only applies when `blast_radius` is also present —
// `category` and `reason` are deliberately generic key names that a
// permission_request hook might use for unrelated purposes, so we
// keep them rendering as plain text when no blast radius indicates
// a safety verdict is in play.
var safetyConventionKeys = map[string]struct{}{
	"blast_radius": {},
	"category":     {},
	"reason":       {},
}

// blastRadiusBadge maps the safer_shell builtin's blast_radius
// vocabulary onto theme colors. Unknown values render unstyled so the
// renderer never silently drops data it doesn't recognise.
func blastRadiusBadge(value string) string {
	style := styles.BaseStyle.Bold(true)
	switch value {
	case "safe":
		style = style.Foreground(styles.Success)
	case "low":
		style = style.Foreground(styles.Success)
	case "medium":
		style = style.Foreground(styles.Warning)
	case "high":
		style = style.Foreground(styles.Error)
	case "unknown":
		style = style.Foreground(styles.TextMuted)
	default:
		return value
	}
	return style.Render(value)
}

// renderSafetyWarning composes the classifier's verdict block from
// the safer_shell metadata. Safe verdicts render a reassuring
// heading; destructive / unknown verdicts render a warning. Returns
// "" when no blast_radius is present.
func (d *toolConfirmationDialog) renderSafetyWarning(contentWidth int) string {
	radius, ok := d.msg.Metadata["blast_radius"]
	if !ok {
		return ""
	}

	var heading string
	if radius == "safe" {
		heading = styles.BaseStyle.Bold(true).Foreground(styles.Success).
			Render("✓ Read-only command — " + blastRadiusBadge(radius))
	} else {
		heading = styles.BaseStyle.Bold(true).Foreground(styles.Warning).
			Render(fmt.Sprintf("⚠  Destructive command — %s blast radius", blastRadiusBadge(radius)))
	}

	lines := []string{heading}
	if reason := d.msg.Metadata["reason"]; reason != "" {
		lines = append(lines, "  "+styles.DialogContentStyle.Render(reason))
	}

	return styles.DialogContentStyle.Width(contentWidth).Render(
		lipgloss.JoinVertical(lipgloss.Left, lines...),
	)
}

// renderMetadata renders the key/value annotations attached to the
// confirmation prompt (static toolset metadata merged with any
// permission_request / preempt-yolo pre_tool_use hook contributions). Returns ""
// when there is none.
//
// Metadata keys the safer_shell builtin uses to express its verdict
// (see [safetyConventionKeys]) are excluded — they're rendered by
// [renderSafetyWarning] as a polished warning block instead of as
// raw key/value rows. Anything else renders as plain text so a
// permission_request hook's freeform annotations still surface.
func (d *toolConfirmationDialog) renderMetadata(contentWidth int) string {
	if len(d.msg.Metadata) == 0 {
		return ""
	}

	// Only suppress convention keys when blast_radius is present —
	// then renderSafetyWarning is composing the message from them.
	// Otherwise (no safety verdict in play), keys like `reason` and
	// `category` are just regular permission_request metadata and
	// should render as plain pairs.
	_, hasBlastRadius := d.msg.Metadata["blast_radius"]

	var lines []string
	for _, k := range slices.Sorted(maps.Keys(d.msg.Metadata)) {
		if hasBlastRadius {
			if _, ok := safetyConventionKeys[k]; ok {
				continue
			}
		}
		key := styles.MutedStyle.Render(k + ": ")
		val := styles.DialogContentStyle.Render(d.msg.Metadata[k])
		lines = append(lines, fmt.Sprintf("  %s%s", key, val))
	}
	if len(lines) == 0 {
		return ""
	}

	header := styles.SecondaryStyle.Render("Metadata")
	lines = append([]string{header}, lines...)
	return styles.DialogContentStyle.Width(contentWidth).Render(
		lipgloss.JoinVertical(lipgloss.Left, lines...),
	)
}

// NewToolConfirmationDialog creates a new tool confirmation dialog
func NewToolConfirmationDialog(msg *runtime.ToolCallConfirmationEvent, sessionState ConfirmationSessionState) Dialog {
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
	pattern := toolconfirm.BuildPermissionPattern(msg.ToolCall)

	return &toolConfirmationDialog{
		msg:               msg,
		sessionState:      sessionState,
		keyMap:            toolconfirm.DefaultKeyMap(),
		scrollView:        scrollView,
		permissionPattern: pattern,
	}
}

// Init initializes the tool confirmation dialog
func (d *toolConfirmationDialog) Init() tea.Cmd {
	return d.scrollView.Init()
}

// executeAction dispatches a confirmation decision.
func (d *toolConfirmationDialog) executeAction(decision toolconfirm.Decision) (layout.Model, tea.Cmd) {
	switch decision {
	case toolconfirm.Approve:
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: toolconfirm.Approve.Resume("", "")}),
		)
	case toolconfirm.Reject:
		return d, core.CmdHandler(OpenDialogMsg{
			Model: NewToolRejectionReasonDialog(),
		})
	case toolconfirm.ApproveTool:
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: toolconfirm.ApproveTool.Resume(d.permissionPattern, "")}),
		)
	case toolconfirm.ApproveBalanced:
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: toolconfirm.ApproveBalanced.Resume("", "")}),
		)
	case toolconfirm.ApproveSession:
		d.sessionState.SetYoloMode(true)
		return d, tea.Sequence(
			core.CmdHandler(CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: toolconfirm.ApproveSession.Resume("", "")}),
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

		if decision, ok := d.keyMap.DecisionFor(msg); ok {
			return d.executeAction(decision)
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

	// Render the help keys and strip ANSI to get plain text for
	// hit-testing. RenderHelpKeys center-aligns within contentWidth, so
	// the stripped string already carries the centering spaces — trim
	// them, or the first segment would start on padding and clicks on
	// the leading action key would dispatch on a space and no-op.
	_, contentWidth := d.dialogDimensions()
	options := d.renderOptions(contentWidth)
	optionsPlain := strings.TrimSpace(ansi.Strip(options))

	// Content starts after left border + padding.
	frameLeft := styles.DialogStyle.GetBorderLeftSize() + styles.DialogStyle.GetPaddingLeft()

	// Re-derive the centering offset from the trimmed content; lipgloss
	// puts the shorter half of an odd padding on the left, matching
	// this floor division.
	plainLen := len(optionsPlain)
	leadingSpaces := max(0, (contentWidth-plainLen)/2)
	relX := msg.X - dialogCol - frameLeft - leadingSpaces
	if relX < 0 || relX >= plainLen {
		return d, nil
	}

	// The help line is built by helpKeysLine as "<KEY> <label>" segments
	// joined with two spaces, e.g.:
	//
	//	"Y yes  N no  T always allow rm*  B auto-approve safe  A all tools"
	//
	// Map the click onto its segment and dispatch on the segment's leading
	// action key. Labels can contain uppercase letters (the always-allow
	// label echoes the command pattern), so scanning the clicked character
	// itself could fire the wrong action. A click in the separator gap
	// resolves to the segment on its left — for the B/A boundary that is
	// also the milder action (Balanced, not Autonomous) when a click near
	// the boundary misses.
	start := 0
	for start < plainLen {
		segEnd := plainLen
		if sep := strings.Index(optionsPlain[start:], "  "); sep >= 0 {
			segEnd = start + sep + 2 // the separator belongs to this segment
		}
		if relX < segEnd {
			if decision, ok := toolconfirm.DecisionForAction(string(optionsPlain[start])); ok {
				return d.executeAction(decision)
			}
			return d, nil
		}
		start = segEnd
	}

	return d, nil
}

// View renders the tool confirmation dialog
func (d *toolConfirmationDialog) View() string {
	dialogWidth, contentWidth := d.dialogDimensions()

	dialogStyle := styles.DialogStyle.Width(dialogWidth)

	titleStyle := styles.DialogTitleStyle.Width(contentWidth)
	title := titleStyle.Render(toolconfirm.Title)

	// Separator
	separator := d.renderSeparator(contentWidth)

	// Get scrollable tool call view
	argumentsSection := d.scrollView.View()

	// Combine all parts with proper spacing
	parts := []string{title, separator}

	if argumentsSection != "" {
		parts = append(parts, "", argumentsSection)
	}

	if warning := d.renderSafetyWarning(contentWidth); warning != "" {
		parts = append(parts, "", warning)
	}

	if metadata := d.renderMetadata(contentWidth); metadata != "" {
		parts = append(parts, "", metadata)
	}

	// Confirmation prompt
	question := styles.DialogQuestionStyle.Width(contentWidth).Render(toolconfirm.Question)
	options := d.renderOptions(contentWidth)

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
