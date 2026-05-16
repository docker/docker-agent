package dialog

import (
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// MultiChoiceOption represents a single selectable option in the dialog.
type MultiChoiceOption struct {
	ID    string // Stable identifier for the option
	Label string // Display label shown to user
	Value string // Value returned when selected (e.g., model-friendly sentence)
}

// MultiChoiceResult holds the result of user selection.
type MultiChoiceResult struct {
	OptionID    string // ID of the selected option ("custom" for custom input, "skip" for no reason)
	Value       string // The value text (option's Value or custom text)
	IsCustom    bool   // True if user provided custom input
	IsSkipped   bool   // True if user chose to skip (no reason)
	IsCancelled bool   // True if user cancelled/escaped
}

// MultiChoiceResultMsg is the tea.Msg sent when the user makes a selection.
type MultiChoiceResultMsg struct {
	DialogID string
	Result   MultiChoiceResult
}

// MultiChoiceConfig configures the multi-choice dialog.
type MultiChoiceConfig struct {
	DialogID          string              // Unique identifier for this dialog instance
	Title             string              // Dialog title (used as the main header)
	Options           []MultiChoiceOption // List of options (max 10 for number selection 0-9)
	AllowCustom       bool                // Whether to allow custom text input
	AllowSecondary    bool                // Whether to allow secondary action (e.g., skip)
	SecondaryLabel    string              // Label for secondary button (default: "Skip")
	PrimaryLabel      string              // Label for primary button (default: "Continue")
	CustomPlaceholder string              // Placeholder for custom input
}

// selection represents which item is currently selected.
type selection int

const (
	selectionNone   selection = -1 // Nothing selected
	selectionCustom selection = -2 // Custom text input selected
	// Values >= 0 represent option indices
)

// Layout constants for multi-choice dialog.
const (
	multiChoiceMinDialogWidth    = 70 // Minimum dialog width (enough for help + buttons)
	multiChoiceMaxDialogWidth    = 85 // Maximum dialog width
	multiChoiceScreenWidthFactor = 90 // Max percentage of screen width (90%)
	multiChoiceMinLabelWidth     = 10 // Minimum width for option labels
	multiChoiceButtonSpacing     = 2  // Spacing between buttons
	multiChoiceMinHelpSpacing    = 2  // Minimum spacing between help text and buttons
)

// indexToDisplayNum converts a 0-based option index to a display number.
// Options 0-8 display as 1-9, option 9 displays as 0.
func indexToDisplayNum(idx int) int {
	if idx == 9 {
		return 0
	}
	return idx + 1
}

// formatKeyRange returns the key range string for help text.
// For 1-9 options: "1-N", for 10 options: "0-9".
func formatKeyRange(numOptions int) string {
	if numOptions >= 10 {
		return "0-9"
	}
	if numOptions == 1 {
		return "1"
	}
	return "1-" + strconv.Itoa(numOptions)
}

// clickableRange stores row range for mouse click detection.
type clickableRange struct {
	startRow  int       // First row (relative to content area start)
	endRow    int       // Last row (inclusive)
	selection selection // What this area selects
}

// multiChoiceDialog implements a reusable multi-choice selection dialog.
type multiChoiceDialog struct {
	BaseDialog

	config            MultiChoiceConfig
	selected          selection       // Currently selected item (-1 = none)
	customInput       textinput.Model // Text input for custom response
	keyMap            multiChoiceKeyMap
	clickables        []clickableRange // Clickable areas for mouse handling (supports multi-row)
	contentAbsRow     int              // Absolute screen row where content starts
	contentAbsCol     int              // Absolute screen column where content starts
	secondaryBtnCol   int              // Column where secondary button starts (relative to content)
	secondaryBtnWidth int              // Width of secondary button
	primaryBtnCol     int              // Column where primary button starts (relative to content)
	primaryBtnWidth   int              // Width of primary button
	btnRow            int              // Row of the buttons (relative to content)
	tabOverride       bool             // When true, inverts the default action (Skip <-> Continue)
}

type multiChoiceKeyMap struct {
	Enter  key.Binding
	Escape key.Binding
	Up     key.Binding
	Down   key.Binding
	Tab    key.Binding
}

func defaultMultiChoiceKeyMap() multiChoiceKeyMap {
	return multiChoiceKeyMap{
		Enter:  key.NewBinding(key.WithKeys("enter")),
		Escape: key.NewBinding(key.WithKeys("esc")),
		Up:     key.NewBinding(key.WithKeys("up")),
		Down:   key.NewBinding(key.WithKeys("down")),
		Tab:    key.NewBinding(key.WithKeys("tab")),
	}
}

// NewMultiChoiceDialog creates a new multi-choice dialog.
func NewMultiChoiceDialog(config MultiChoiceConfig) Dialog {
	// Set defaults
	if config.SecondaryLabel == "" {
		config.SecondaryLabel = "Skip"
	}
	if config.PrimaryLabel == "" {
		config.PrimaryLabel = "Continue"
	}
	if config.CustomPlaceholder == "" {
		config.CustomPlaceholder = "Other..."
	}

	// Create text input
	ti := textinput.New()
	ti.SetStyles(styles.DialogInputStyle)
	ti.Placeholder = config.CustomPlaceholder
	ti.CharLimit = 500
	ti.SetWidth(50)
	ti.Prompt = ""

	return &multiChoiceDialog{
		config:      config,
		selected:    selectionNone, // Nothing selected by default
		customInput: ti,
		keyMap:      defaultMultiChoiceKeyMap(),
	}
}

// hasSelection returns true if user has made a selection or typed something.
func (d *multiChoiceDialog) hasSelection() bool {
	if d.selected >= 0 {
		return true // Option selected
	}
	if d.selected == selectionCustom && strings.TrimSpace(d.customInput.Value()) != "" {
		return true // Custom text entered
	}
	return false
}

// isSecondaryDefault returns true if secondary button should be the default action.
// This takes into account the natural default (based on selection) and the tabOverride.
func (d *multiChoiceDialog) isSecondaryDefault() bool {
	naturalDefault := !d.hasSelection() // Secondary is natural default when nothing selected
	if d.tabOverride {
		return !naturalDefault // Invert when tab override is active
	}
	return naturalDefault
}

// renderNumberBox renders a number box and returns its width.
func (d *multiChoiceDialog) renderNumberBox(num int) (rendered string, width int) {
	numStyle := styles.DialogContentStyle.Foreground(styles.TextMuted)
	numStr := strconv.Itoa(num)
	rendered = numStyle.Padding(0, 1).Render(numStr)
	return rendered, lipgloss.Width(rendered)
}

// computeHelpAndButtonsWidth calculates the actual width of help text and buttons.
func (d *multiChoiceDialog) computeHelpAndButtonsWidth() int {
	// Build the actual help text
	helpStyle := styles.DialogHelpStyle
	keyStyle := helpStyle.Foreground(styles.TextSecondary)

	numOptions := len(d.config.Options)
	if d.config.AllowCustom {
		numOptions++
	}

	helpParts := []string{
		keyStyle.Render("Esc") + " " + helpStyle.Render("cancel"),
	}
	if numOptions > 0 {
		helpParts = append(helpParts, keyStyle.Render("↑/↓ "+formatKeyRange(numOptions))+" "+helpStyle.Render("select"))
	} else {
		helpParts = append(helpParts, keyStyle.Render("↑/↓")+" "+helpStyle.Render("navigate"))
	}
	helpText := strings.Join(helpParts, "  ")
	helpWidth := lipgloss.Width(helpText)

	// Build the actual buttons (use longer variant with ↵ for measurement)
	btnStyle := lipgloss.NewStyle().Padding(0, 2)
	secondaryBtn := btnStyle.Render(d.config.SecondaryLabel + " ↵")
	primaryBtn := btnStyle.Render(d.config.PrimaryLabel + " ↵")
	btnWidth := lipgloss.Width(secondaryBtn) + multiChoiceButtonSpacing + lipgloss.Width(primaryBtn)

	return helpWidth + multiChoiceMinHelpSpacing + btnWidth
}

// computeDialogWidth calculates optimal dialog width based on content.
func (d *multiChoiceDialog) computeDialogWidth() int {
	frameWidth := styles.DialogStyle.GetHorizontalFrameSize()

	// Compute number box width from actual rendering (use highest display number)
	// Display numbers are 1-9, then 0 for the 10th option
	_, numBoxWidth := d.renderNumberBox(9) // max single digit is 9

	maxContentWidth := 0

	// Check title width
	titleWidth := lipgloss.Width(d.config.Title)
	if titleWidth > maxContentWidth {
		maxContentWidth = titleWidth
	}

	// Check each option width (number box + space + label)
	for _, opt := range d.config.Options {
		optWidth := numBoxWidth + 1 + lipgloss.Width(opt.Label) // +1 for space
		if optWidth > maxContentWidth {
			maxContentWidth = optWidth
		}
	}

	// Check custom placeholder width
	if d.config.AllowCustom {
		// +1 for space, +1 for cursor space
		customWidth := numBoxWidth + 1 + lipgloss.Width(d.config.CustomPlaceholder) + 1
		if customWidth > maxContentWidth {
			maxContentWidth = customWidth
		}
	}

	// Calculate help + buttons width from actual rendering
	helpAndBtnWidth := d.computeHelpAndButtonsWidth()
	if helpAndBtnWidth > maxContentWidth {
		maxContentWidth = helpAndBtnWidth
	}

	// Calculate total dialog width
	dialogWidth := min(max(maxContentWidth+frameWidth, multiChoiceMinDialogWidth), multiChoiceMaxDialogWidth)

	// Don't exceed screen width
	screenLimit := d.Width() * multiChoiceScreenWidthFactor / 100
	if dialogWidth > screenLimit && screenLimit > multiChoiceMinDialogWidth {
		dialogWidth = screenLimit
	}

	return dialogWidth
}

// Init initializes the dialog.
func (d *multiChoiceDialog) Init() tea.Cmd {
	return textinput.Blink
}

// Update handles messages.
func (d *multiChoiceDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		// Forward paste to custom text input if custom is selected or allowed
		if d.selected == selectionCustom {
			var cmd tea.Cmd
			d.customInput, cmd = d.customInput.Update(msg)
			return d, cmd
		}
		// Auto-select custom mode when pasting if custom is allowed
		if d.config.AllowCustom {
			d.selected = selectionCustom
			d.customInput.Focus()
			var cmd tea.Cmd
			d.customInput, cmd = d.customInput.Update(msg)
			return d, cmd
		}
		return d, nil

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		return d.handleKeyPress(msg)

	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return d.handleMouseClick(msg.X, msg.Y)
		}
	}

	return d, nil
}

// handleKeyPress handles key presses.
func (d *multiChoiceDialog) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	keyStr := msg.String()

	// If custom is selected, forward most keys to the text input (including numbers)
	if d.selected == selectionCustom {
		switch {
		case key.Matches(msg, d.keyMap.Escape):
			cmd := d.sendResult(MultiChoiceResult{IsCancelled: true})
			return d, cmd
		case key.Matches(msg, d.keyMap.Enter):
			return d.submitDefault()
		case key.Matches(msg, d.keyMap.Up):
			d.selectPrevious()
			return d, nil
		case key.Matches(msg, d.keyMap.Down):
			d.selectNext()
			return d, nil
		case key.Matches(msg, d.keyMap.Tab):
			d.tabOverride = !d.tabOverride
			return d, nil
		default:
			// Forward to text input (including number keys)
			var cmd tea.Cmd
			d.customInput, cmd = d.customInput.Update(msg)
			return d, cmd
		}
	}

	// Check for number shortcuts (1-9, 0 for 10th) - select that option or custom
	// Only when NOT in custom mode
	// Keys 1-9 select options at indices 0-8, key 0 selects option at index 9
	if len(keyStr) == 1 && keyStr[0] >= '0' && keyStr[0] <= '9' {
		var optIdx int
		if keyStr[0] == '0' {
			optIdx = 9 // '0' is the 10th option
		} else {
			optIdx = int(keyStr[0] - '1') // '1' -> 0, '2' -> 1, etc.
		}
		if optIdx < len(d.config.Options) {
			d.selected = selection(optIdx)
			d.customInput.Blur()
			return d, nil
		}
		// Check if this is the custom option number
		if d.config.AllowCustom && optIdx == len(d.config.Options) {
			d.selected = selectionCustom
			d.customInput.Focus()
			return d, nil
		}
	}

	switch {
	case key.Matches(msg, d.keyMap.Escape):
		cmd := d.sendResult(MultiChoiceResult{IsCancelled: true})
		return d, cmd

	case key.Matches(msg, d.keyMap.Enter):
		return d.submitDefault()

	case key.Matches(msg, d.keyMap.Up):
		d.selectPrevious()
		return d, nil

	case key.Matches(msg, d.keyMap.Down):
		d.selectNext()
		return d, nil

	case key.Matches(msg, d.keyMap.Tab):
		d.tabOverride = !d.tabOverride
		return d, nil

	default:
		// If custom is allowed and user types, auto-select custom
		if d.config.AllowCustom {
			if len(keyStr) == 1 || keyStr == "backspace" || keyStr == "delete" {
				d.selected = selectionCustom
				d.customInput.Focus()
				var cmd tea.Cmd
				d.customInput, cmd = d.customInput.Update(msg)
				return d, cmd
			}
		}
		return d, nil
	}
}

// selectPrevious moves selection up.
func (d *multiChoiceDialog) selectPrevious() {
	switch d.selected {
	case selectionNone:
		// From none, go to custom if allowed, otherwise last option
		if d.config.AllowCustom {
			d.selected = selectionCustom
		} else if len(d.config.Options) > 0 {
			d.selected = selection(len(d.config.Options) - 1)
		}
	case selectionCustom:
		if len(d.config.Options) > 0 {
			d.selected = selection(len(d.config.Options) - 1)
		} else {
			d.selected = selectionNone
		}
	default:
		if int(d.selected) > 0 {
			d.selected--
		} else {
			d.selected = selectionNone
		}
	}
	d.updateFocus()
}

// selectNext moves selection down.
func (d *multiChoiceDialog) selectNext() {
	maxOpt := len(d.config.Options) - 1

	switch d.selected {
	case selectionNone:
		if len(d.config.Options) > 0 {
			d.selected = 0
		} else if d.config.AllowCustom {
			d.selected = selectionCustom
		}
	case selectionCustom:
		d.selected = selectionNone
	default:
		switch {
		case int(d.selected) < maxOpt:
			d.selected++
		case d.config.AllowCustom:
			d.selected = selectionCustom
		default:
			d.selected = selectionNone
		}
	}
	d.updateFocus()
}

// updateFocus manages text input focus based on selection.
func (d *multiChoiceDialog) updateFocus() {
	if d.selected == selectionCustom {
		d.customInput.Focus()
	} else {
		d.customInput.Blur()
	}
}

// submitDefault submits based on current state and tab override.
// Secondary is default when nothing selected (unless tab toggled).
// Primary is default when selection exists (unless tab toggled).
func (d *multiChoiceDialog) submitDefault() (layout.Model, tea.Cmd) {
	if d.isSecondaryDefault() {
		return d.submitSecondary()
	}
	return d.submitPrimary()
}

// submitSecondary sends a secondary (skip) result.
func (d *multiChoiceDialog) submitSecondary() (layout.Model, tea.Cmd) {
	if !d.config.AllowSecondary {
		return d, nil
	}
	cmd := d.sendResult(MultiChoiceResult{
		OptionID:  "skip",
		IsSkipped: true,
	})
	return d, cmd
}

// submitPrimary sends the current selection.
func (d *multiChoiceDialog) submitPrimary() (layout.Model, tea.Cmd) {
	switch d.selected {
	case selectionNone:
		// Nothing selected - if secondary allowed, use it; otherwise do nothing
		if d.config.AllowSecondary {
			return d.submitSecondary()
		}
		return d, nil
	case selectionCustom:
		value := strings.TrimSpace(d.customInput.Value())
		if value == "" {
			// Empty custom = secondary if allowed
			if d.config.AllowSecondary {
				return d.submitSecondary()
			}
			return d, nil
		}
		cmd := d.sendResult(MultiChoiceResult{
			OptionID: "custom",
			Value:    value,
			IsCustom: true,
		})
		return d, cmd
	default:
		if int(d.selected) >= 0 && int(d.selected) < len(d.config.Options) {
			opt := d.config.Options[d.selected]
			cmd := d.sendResult(MultiChoiceResult{
				OptionID: opt.ID,
				Value:    opt.Value,
			})
			return d, cmd
		}
	}
	return d, nil
}

// handleMouseClick handles mouse clicks.
func (d *multiChoiceDialog) handleMouseClick(x, y int) (layout.Model, tea.Cmd) {
	relY := y - d.contentAbsRow
	relX := x - d.contentAbsCol

	// Check buttons
	if relY == d.btnRow {
		// Skip button
		if relX >= d.secondaryBtnCol && relX < d.secondaryBtnCol+d.secondaryBtnWidth {
			return d.submitSecondary()
		}
		// Primary button
		if relX >= d.primaryBtnCol && relX < d.primaryBtnCol+d.primaryBtnWidth {
			return d.submitPrimary()
		}
	}

	// Check clickable areas (options) - now supports row ranges for word wrap
	for _, area := range d.clickables {
		if relY >= area.startRow && relY <= area.endRow {
			if d.selected == area.selection {
				// Already selected - deselect
				d.selected = selectionNone
			} else {
				d.selected = area.selection
			}
			d.updateFocus()
			return d, nil
		}
	}

	return d, nil
}

// sendResult creates the command to close dialog and send result.
func (d *multiChoiceDialog) sendResult(result MultiChoiceResult) tea.Cmd {
	return tea.Sequence(
		core.CmdHandler(CloseDialogMsg{}),
		core.CmdHandler(MultiChoiceResultMsg{
			DialogID: d.config.DialogID,
			Result:   result,
		}),
	)
}

// View renders the dialog.
func (d *multiChoiceDialog) View() string {
	dialogWidth := d.computeDialogWidth()
	contentWidth := d.ContentWidth(dialogWidth, 2)

	content := NewContent(contentWidth)
	content.AddTitle(d.config.Title)
	content.AddSeparator()

	// Reset clickables
	d.clickables = nil
	rowIdx := 0

	// Render options with number keys (1-indexed, 0 for 10th)
	for i, opt := range d.config.Options {
		isSelected := d.selected == selection(i)
		displayNum := indexToDisplayNum(i)
		line := d.renderOption(displayNum, opt.Label, isSelected, contentWidth)
		content.AddContent(line)

		// Calculate how many rows this option takes
		lineHeight := lipgloss.Height(line)
		d.clickables = append(d.clickables, clickableRange{
			startRow:  rowIdx,
			endRow:    rowIdx + lineHeight - 1,
			selection: selection(i),
		})
		rowIdx += lineHeight
	}

	// Render custom input if allowed (as another option)
	if d.config.AllowCustom {
		isSelected := d.selected == selectionCustom
		customLine := d.renderCustomOption(isSelected, contentWidth)
		content.AddContent(customLine)

		lineHeight := lipgloss.Height(customLine)
		d.clickables = append(d.clickables, clickableRange{
			startRow:  rowIdx,
			endRow:    rowIdx + lineHeight - 1,
			selection: selectionCustom,
		})
		rowIdx += lineHeight
	}

	// Spacing before help/buttons
	content.AddSpace()
	rowIdx++
	content.AddSpace()
	rowIdx++

	// Help text and buttons on same row
	helpAndButtons := d.renderHelpAndButtons(contentWidth)
	content.AddContent(helpAndButtons)
	// Cache the button row index so click handling in Update() can hit-test
	// against the same layout that was just rendered.
	d.btnRow = rowIdx //rubocop:disable Lint/TUIViewPurity // click-zone cache consumed by Update()

	return styles.DialogStyle.Width(dialogWidth).Render(content.Build())
}

// renderOption renders a numbered option with selection indicator, with word wrap support.
func (d *multiChoiceDialog) renderOption(num int, label string, isSelected bool, contentWidth int) string {
	// Determine if we should fade this option (another option is selected)
	hasAnySelection := d.hasSelection()
	isFaded := hasAnySelection && !isSelected

	numStyle := styles.DialogContentStyle.Foreground(styles.TextMuted)
	labelStyle := styles.DialogContentStyle.Foreground(styles.TextPrimary)
	selectedNumStyle := styles.DialogContentStyle.Foreground(styles.Background).Background(styles.Accent).Bold(true)
	selectedLabelStyle := styles.DialogContentStyle.Foreground(styles.TextPrimary)
	fadedNumStyle := styles.DialogContentStyle.Foreground(styles.TextMutedGray)
	fadedLabelStyle := styles.DialogContentStyle.Foreground(styles.TextSecondary)

	numStr := strconv.Itoa(num)
	var numBox string
	switch {
	case isSelected:
		numBox = selectedNumStyle.Padding(0, 1).Render(numStr)
	case isFaded:
		numBox = fadedNumStyle.Padding(0, 1).Render(numStr)
	default:
		numBox = numStyle.Padding(0, 1).Render(numStr)
	}
	numBoxWidth := lipgloss.Width(numBox)

	// Calculate available width for label (allow word wrap)
	labelWidth := max(
		// -1 for space between number box and label
		contentWidth-numBoxWidth-1, multiChoiceMinLabelWidth)

	// Apply width constraint for word wrapping
	var labelRendered string
	switch {
	case isSelected:
		labelRendered = selectedLabelStyle.Width(labelWidth).Render(label)
	case isFaded:
		labelRendered = fadedLabelStyle.Width(labelWidth).Render(label)
	default:
		labelRendered = labelStyle.Width(labelWidth).Render(label)
	}

	return numBox + " " + labelRendered
}

// renderCustomOption renders the custom text input as an option.
// contentWidth should be passed from the caller to avoid recomputing dialog dimensions.
func (d *multiChoiceDialog) renderCustomOption(isSelected bool, contentWidth int) string {
	// Determine if we should fade this option (another option is selected)
	hasAnySelection := d.hasSelection()
	isFaded := hasAnySelection && !isSelected

	numStyle := styles.DialogContentStyle.Foreground(styles.TextMuted)
	selectedNumStyle := styles.DialogContentStyle.Foreground(styles.Background).Background(styles.Accent).Bold(true)
	fadedNumStyle := styles.DialogContentStyle.Foreground(styles.TextMutedGray)

	// Custom option is numbered after all regular options (1-indexed, 0 for 10th)
	displayNum := indexToDisplayNum(len(d.config.Options))
	numStr := strconv.Itoa(displayNum)
	var numBox string
	switch {
	case isSelected:
		numBox = selectedNumStyle.Padding(0, 1).Render(numStr)
	case isFaded:
		numBox = fadedNumStyle.Padding(0, 1).Render(numStr)
	default:
		numBox = numStyle.Padding(0, 1).Render(numStr)
	}
	numBoxWidth := lipgloss.Width(numBox)

	// Calculate available width for text display
	// -1 for space between number box and input, -1 for cursor space
	availableWidth := max(contentWidth-numBoxWidth-2, multiChoiceMinLabelWidth)

	value := d.customInput.Value()

	if isSelected {
		// Set width and let textinput handle its own scrolling/viewport
		d.customInput.SetWidth(availableWidth)
		return numBox + " " + d.customInput.View()
	}

	// When not selected, show truncated text with ellipsis if too long
	if value == "" {
		placeholderStyle := styles.DialogContentStyle.Foreground(styles.TextMuted).Italic(true)
		if isFaded {
			placeholderStyle = styles.DialogContentStyle.Foreground(styles.TextMutedGray).Italic(true)
		}
		return numBox + " " + placeholderStyle.Render(d.config.CustomPlaceholder)
	}

	// Truncate with ellipsis at end (showing beginning of text when not selected)
	textStyle := styles.DialogContentStyle.Foreground(styles.TextPrimary)
	if isFaded {
		textStyle = styles.DialogContentStyle.Foreground(styles.TextSecondary)
	}
	displayText := truncateWithEllipsisEnd(value, availableWidth)
	return numBox + " " + textStyle.Render(displayText)
}

// truncateWithEllipsisEnd truncates text to fit within maxWidth, adding "..." at the end.
func truncateWithEllipsisEnd(text string, maxWidth int) string {
	textWidth := lipgloss.Width(text)
	if textWidth <= maxWidth {
		return text
	}

	// Need to truncate - show beginning of text with "..." suffix
	const ellipsis = "..."
	ellipsisWidth := lipgloss.Width(ellipsis)
	availableForText := maxWidth - ellipsisWidth
	if availableForText < 1 {
		return ellipsis
	}

	// Take characters from the beginning
	result := ""
	for _, r := range text {
		candidate := result + string(r)
		if lipgloss.Width(candidate) > availableForText {
			break
		}
		result = candidate
	}

	return result + ellipsis
}

// renderHelpAndButtons renders help text on left and buttons on right.
func (d *multiChoiceDialog) renderHelpAndButtons(contentWidth int) string {
	secondaryIsDefault := d.isSecondaryDefault()

	// Help text
	helpStyle := styles.DialogHelpStyle
	keyStyle := helpStyle.Foreground(styles.TextSecondary)

	numOptions := len(d.config.Options)
	if d.config.AllowCustom {
		numOptions++
	}

	helpParts := []string{
		keyStyle.Render("Esc") + " " + helpStyle.Render("cancel"),
	}
	if numOptions > 0 {
		helpParts = append(helpParts, keyStyle.Render("↑/↓ "+formatKeyRange(numOptions))+" "+helpStyle.Render("select"))
	} else {
		helpParts = append(helpParts, keyStyle.Render("↑/↓")+" "+helpStyle.Render("navigate"))
	}
	helpText := strings.Join(helpParts, "  ")

	// Button styles
	defaultBtnStyle := lipgloss.NewStyle().
		Foreground(styles.Background).
		Background(styles.Accent).
		Padding(0, 2).
		Bold(true)

	normalBtnStyle := lipgloss.NewStyle().
		Foreground(styles.TextMuted).
		Padding(0, 2)

	var secondaryBtn, primaryBtn string

	if secondaryIsDefault {
		// Secondary is default
		secondaryBtn = defaultBtnStyle.Render(d.config.SecondaryLabel + " ↵")
		primaryBtn = normalBtnStyle.Render(d.config.PrimaryLabel)
	} else {
		// Primary is default
		secondaryBtn = normalBtnStyle.Render(d.config.SecondaryLabel)
		primaryBtn = defaultBtnStyle.Render(d.config.PrimaryLabel + " ↵")
	}

	// Calculate widths
	helpWidth := lipgloss.Width(helpText)
	secondaryWidth := lipgloss.Width(secondaryBtn)
	primaryWidth := lipgloss.Width(primaryBtn)
	totalBtnWidth := secondaryWidth + multiChoiceButtonSpacing + primaryWidth

	// Calculate spacing between help and buttons
	spacing := max(contentWidth-helpWidth-totalBtnWidth, multiChoiceMinHelpSpacing)

	// Store button positions for click detection (relative to content area)
	d.secondaryBtnCol = helpWidth + spacing
	d.secondaryBtnWidth = secondaryWidth
	d.primaryBtnCol = d.secondaryBtnCol + secondaryWidth + multiChoiceButtonSpacing
	d.primaryBtnWidth = primaryWidth

	return helpText + strings.Repeat(" ", spacing) + secondaryBtn + strings.Repeat(" ", multiChoiceButtonSpacing) + primaryBtn
}

// Position returns the dialog position (centered).
func (d *multiChoiceDialog) Position() (row, col int) {
	dialogWidth := d.computeDialogWidth()
	contentWidth := d.ContentWidth(dialogWidth, 2)
	renderedDialog := d.View()
	dialogHeight := lipgloss.Height(renderedDialog)
	row, col = CenterPosition(d.Width(), d.Height(), dialogWidth, dialogHeight)

	// Calculate absolute position of content area using style getters
	borderTop := styles.DialogStyle.GetBorderTopSize()
	borderLeft := styles.DialogStyle.GetBorderLeftSize()
	paddingTop := styles.DialogStyle.GetPaddingTop()
	paddingLeft := styles.DialogStyle.GetPaddingLeft()

	contentRow := row + borderTop + paddingTop
	contentCol := col + borderLeft + paddingLeft

	// Title
	titleStyle := styles.DialogTitleStyle.Width(contentWidth)
	title := titleStyle.Render(d.config.Title)
	contentRow += lipgloss.Height(title)

	// Separator
	separatorHeight := lipgloss.Height(RenderSeparator(contentWidth))
	contentRow += separatorHeight

	d.contentAbsRow = contentRow
	d.contentAbsCol = contentCol

	return row, col
}
