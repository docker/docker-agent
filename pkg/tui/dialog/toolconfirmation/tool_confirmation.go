// Package toolconfirmation provides tool approval and rejection dialogs.
package toolconfirmation

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/messages"
	"github.com/docker/docker-agent/pkg/tui/components/toolconfirm"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog/common"
	tuimessages "github.com/docker/docker-agent/pkg/tui/messages"
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

	// toolConfirmMinOptionWidth is the narrowest width the option block
	// can be laid out or rendered at: one action key, the key/label gap,
	// and the "…" a fully truncated label collapses to. Anything narrower
	// wraps a minimal segment across physical lines, breaking the
	// one-line-per-row contract click hit-testing relies on.
	toolConfirmMinOptionWidth = 3
)

type (
	RuntimeResumeMsg struct {
		Request runtime.ResumeRequest
	}
)

// Response represents the user's response to tool confirmation
type Response struct {
	Response string // "approve", "reject", or "approve-session"
}

// ConfirmationSessionState is the session-state surface the confirmation
// dialog needs: the message list's surface for rendering the tool call, plus
// the session-wide approval flip for the "all tools" decision.
type ConfirmationSessionState interface {
	messages.SessionState
	SetYoloMode(yoloMode bool)
}

type toolConfirmationDialog struct {
	common.BaseDialog

	msg               *runtime.ToolCallConfirmationEvent
	keyMap            toolconfirm.KeyMap
	sessionState      ConfirmationSessionState
	scrollView        messages.Model
	permissionPattern string // cached permission pattern for this tool call
}

// dialogDimensions returns computed dialog width and content width.
// contentWidth never drops below toolConfirmMinOptionWidth: the outer
// dialog style re-wraps its content at contentWidth, so shrinking
// further would split even a minimal option row across physical lines
// and misalign click hit-testing. An extremely narrow terminal clips
// the dialog instead, which is the lesser evil. SetSize, View, Position
// and handleMouseClick all derive from this one clamped layout.
func (d *toolConfirmationDialog) dialogDimensions() (dialogWidth, contentWidth int) {
	frameWidth := styles.DialogStyle.GetHorizontalFrameSize()
	dialogWidth = max(d.Width()*toolConfirmDialogWidthPercent/100, toolConfirmMinOptionWidth+frameWidth)
	contentWidth = dialogWidth - frameWidth
	return dialogWidth, contentWidth
}

// SetSize implements [common.Dialog].
func (d *toolConfirmationDialog) SetSize(width, height int) tea.Cmd {
	d.BaseDialog.SetSize(width, height)

	// Calculate dialog dimensions using helper
	_, contentWidth := d.dialogDimensions()
	maxDialogHeight := height * toolConfirmDialogHeightPercent / 100

	// Measure fixed UI elements using the same rendering as View()
	titleStyle := styles.DialogTitleStyle.Width(contentWidth)
	title := titleStyle.Render(toolconfirm.Title)
	titleHeight := lipgloss.Height(title)

	separator := common.RenderSeparator(contentWidth)
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

// renderOptions renders the Y/N/T/B/A decision block: the option
// segments laid out by optionRows, one centered help-keys line per row.
// The width shares optionRows' clamp so a laid-out row never wraps
// inside common.RenderHelpKeys.
func (d *toolConfirmationDialog) renderOptions(contentWidth int) string {
	contentWidth = max(contentWidth, toolConfirmMinOptionWidth)
	rows := d.optionRows(contentWidth)
	lines := make([]string, len(rows))
	for i, row := range rows {
		bindings := make([]string, 0, len(row)*2)
		for _, seg := range row {
			bindings = append(bindings, seg.action, seg.label)
		}
		lines[i] = common.RenderHelpKeys(contentWidth, bindings...)
	}
	return strings.Join(lines, "\n")
}

// optionRows lays out the decision segments for the current permission
// pattern. renderOptions and handleMouseClick both derive from this one
// layout, so what the user sees and what a click hits cannot drift apart.
// The width is clamped so a minimal "<key> …" segment always fits on one
// row, no matter how narrow the caller's width is.
func (d *toolConfirmationDialog) optionRows(contentWidth int) [][]optionSegment {
	return layoutOptionRows(max(contentWidth, toolConfirmMinOptionWidth), toolconfirm.OptionsHelp(d.permissionPattern))
}

// optionSegment is one clickable "<key> <label>" unit of the decision
// block. startX/endX are terminal cell offsets of the segment within its
// row's visible text (centering padding excluded), endX exclusive.
type optionSegment struct {
	action string // action key, dispatched via toolconfirm.DecisionForAction
	label  string // displayed label; truncated when the segment alone exceeds contentWidth
	startX int
	endX   int
}

// layoutOptionRows greedily packs "<key> <label>" segments into rows at
// most contentWidth cells wide, breaking only at the two-space segment
// separators so no segment ever splits across rows. A segment too wide
// even for a row of its own gets its displayed label truncated with an
// ellipsis — the action key stays visible and the underlying permission
// pattern is unaffected. All widths are terminal cell widths, not byte
// lengths (labels and the ellipsis can hold multi-byte runes). Callers
// clamp contentWidth to toolConfirmMinOptionWidth or more, so even a
// fully truncated "<key> …" segment fits its row.
func layoutOptionRows(contentWidth int, bindings []string) [][]optionSegment {
	if len(bindings) == 0 || len(bindings)%2 != 0 {
		return nil
	}
	var rows [][]optionSegment
	var row []optionSegment
	rowWidth := 0
	for i := 0; i < len(bindings); i += 2 {
		action, label := bindings[i], bindings[i+1]
		keyWidth := ansi.StringWidth(action)
		if keyWidth+1+ansi.StringWidth(label) > contentWidth {
			label = ansi.Truncate(label, max(contentWidth-keyWidth-1, 1), "…")
		}
		segWidth := keyWidth + 1 + ansi.StringWidth(label)
		if len(row) > 0 && rowWidth+2+segWidth > contentWidth {
			rows = append(rows, row)
			row, rowWidth = nil, 0
		}
		startX := 0
		if len(row) > 0 {
			startX = rowWidth + 2
		}
		row = append(row, optionSegment{action: action, label: label, startX: startX, endX: startX + segWidth})
		rowWidth = startX + segWidth
	}
	return append(rows, row)
}

// optionRowText is the plain text of one laid-out row — exactly what
// helpKeysLine renders for its bindings, without styling.
func optionRowText(row []optionSegment) string {
	parts := make([]string, len(row))
	for i, seg := range row {
		parts[i] = seg.action + " " + seg.label
	}
	return strings.Join(parts, "  ")
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
	"safety_label": {},
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
	switch radius {
	case "safe":
		heading = styles.BaseStyle.Bold(true).Foreground(styles.Success).
			Render("✓ Read-only command — " + blastRadiusBadge(radius))
	case "unknown":
		// Not positively recognised ≠ destructive: crying wolf on every
		// unlisted command would teach users to ignore the real warnings.
		heading = styles.BaseStyle.Bold(true).Foreground(styles.Warning).
			Render("⚠  Unrecognised command — not classified as safe")
	default:
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
func NewToolConfirmationDialog(ar *animation.Runtime, msg *runtime.ToolCallConfirmationEvent, sessionState ConfirmationSessionState, opts ...messages.Option) common.Dialog {
	// Create scrollable view with minimal initial size (will be updated in SetSize)
	scrollView := messages.NewScrollableView(ar, 1, 1, sessionState, opts...)

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
			core.CmdHandler(common.CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: toolconfirm.Approve.Resume("", "")}),
		)
	case toolconfirm.Reject:
		return d, core.CmdHandler(common.OpenDialogMsg{
			Model: NewToolRejectionReasonDialog(),
		})
	case toolconfirm.ApproveTool:
		return d, tea.Sequence(
			core.CmdHandler(common.CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: toolconfirm.ApproveTool.Resume(d.permissionPattern, "")}),
		)
	case toolconfirm.ApproveBalanced:
		// A preempt/custom hook can raise a confirmation while the session
		// is autonomous. Balanced moves the runtime session off autonomous,
		// so the cached TUI flag must drop too — otherwise the sidebar keeps
		// claiming YOLO and Ctrl+Y "toggles" it straight back to autonomous.
		// Mirrors ApproveSession setting it true below.
		d.sessionState.SetYoloMode(false)
		return d, tea.Sequence(
			core.CmdHandler(common.CloseDialogMsg{}),
			core.CmdHandler(RuntimeResumeMsg{Request: toolconfirm.ApproveBalanced.Resume("", "")}),
		)
	case toolconfirm.ApproveSession:
		d.sessionState.SetYoloMode(true)
		return d, tea.Sequence(
			core.CmdHandler(common.CloseDialogMsg{}),
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
		if cmd := common.HandleQuit(msg); cmd != nil {
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

// handleMouseClick handles mouse clicks on the action buttons (Y/N/T/B/A).
func (d *toolConfirmationDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	dialogRow, dialogCol := d.Position()
	renderedDialog := d.View()
	dialogHeight := lipgloss.Height(renderedDialog)

	// The option rows are the last content lines inside the dialog: View
	// appends renderOptions last, one physical line per laid-out row.
	_, contentWidth := d.dialogDimensions()
	rows := d.optionRows(contentWidth)
	rowIdx := msg.Y - common.ContentEndRow(dialogRow, dialogHeight) + len(rows) - 1
	if rowIdx < 0 || rowIdx >= len(rows) {
		return d, nil
	}
	row := rows[rowIdx]

	// Hit-test against the line the user actually saw: take the clicked
	// row from the rendered dialog and locate the row's text inside it.
	// Reconstructing the left offset from frame sizes plus the centering
	// padding is fragile — any extra inset between the frame and the
	// help line shifts every click. The matched byte position is turned
	// into a cell offset (the border rune is multi-byte, and labels can
	// be too), because mouse coordinates are terminal cells.
	renderedLines := strings.Split(ansi.Strip(renderedDialog), "\n")
	lineIdx := msg.Y - dialogRow
	if lineIdx < 0 || lineIdx >= len(renderedLines) {
		return d, nil
	}
	line := renderedLines[lineIdx]
	byteStart := strings.Index(line, optionRowText(row))
	if byteStart < 0 {
		return d, nil
	}
	relX := msg.X - dialogCol - ansi.StringWidth(line[:byteStart])

	// Map the click onto its segment and dispatch the segment's action
	// key — never a character parsed out of the label (labels can contain
	// uppercase letters; the always-allow label echoes the command
	// pattern). Separator gaps are dead zones: attributing them to either
	// neighbour would fire some action on a near-miss, and no attribution
	// is uniformly the safer one (left makes the Y/N gap approve, right
	// makes the B/A gap go autonomous).
	for _, seg := range row {
		if relX >= seg.startX && relX < seg.endX {
			if decision, ok := toolconfirm.DecisionForAction(seg.action); ok {
				return d.executeAction(decision)
			}
			return d, nil
		}
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
	separator := common.RenderSeparator(contentWidth)

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
	return common.CenterPosition(d.Width(), d.Height(), dialogWidth, dialogHeight)
}
