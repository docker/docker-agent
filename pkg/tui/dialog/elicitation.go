package dialog

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	defaultCharLimit = 500
	numberCharLimit  = 50
	defaultWidth     = 50

	// elicitationHeaderLines is the count of fixed header lines above the
	// scrollable body (title + separator).
	elicitationHeaderLines = 2
	// elicitationOverhead is the dialog height not available to the body:
	// header (2) + footer blank+help (2) + frame border+padding (4).
	elicitationOverhead = 8
)

// ElicitationField represents a form field extracted from a JSON schema.
type ElicitationField struct {
	Name, Title, Type, Description string
	Required                       bool
	EnumValues                     []string
	Default                        any
	MinLength, MaxLength           int
	Format, Pattern                string
	Minimum, Maximum               float64
	HasMinimum, HasMaximum         bool
}

// ElicitationDialog implements Dialog for MCP elicitation requests.
//
// When a schema is provided, fields are rendered as a form.
// When no schema is provided, a single free-form text input (responseInput)
// is shown so the user can type an answer.
//
// The body region (message + fields, or message + free-form input) is
// rendered inside a scrollview so long content remains accessible when it
// would otherwise overflow the terminal.
type ElicitationDialog struct {
	BaseDialog

	title         string
	message       string
	fields        []ElicitationField
	inputs        []textinput.Model
	boolValues    map[int]bool
	enumIndexes   map[int]int // selected index for enum fields
	currentField  int
	keyMap        elicitationKeyMap
	fieldErrors   map[int]string  // validation error messages per field
	responseInput textinput.Model // free-form text input used when len(fields) == 0

	scrollview *scrollview.Model
	// fieldStarts[i] is the line offset of field i's label inside the
	// scrollable body. Populated by View() / Position().
	fieldStarts []int
	// scrollableRow is the absolute screen row of the first scrollable line.
	scrollableRow int
}

type elicitationKeyMap struct {
	Up, Down, Tab, ShiftTab, Enter, Escape, Space key.Binding
}

// hasFreeFormInput returns true when no schema fields exist and the dialog
// shows a single free-form text input instead.
func (d *ElicitationDialog) hasFreeFormInput() bool {
	return len(d.fields) == 0
}

// NewElicitationDialog creates a new elicitation dialog.
func NewElicitationDialog(message string, schema any, meta map[string]any) Dialog {
	fields := parseElicitationSchema(schema)

	// Determine dialog title from meta, defaulting to "Question"
	title := "Question"
	if meta != nil {
		if t, ok := meta["cagent/title"].(string); ok && t != "" {
			title = t
		}
	}

	d := &ElicitationDialog{
		title:       title,
		message:     message,
		fields:      fields,
		inputs:      make([]textinput.Model, len(fields)),
		boolValues:  make(map[int]bool),
		enumIndexes: make(map[int]int),
		fieldErrors: make(map[int]string),
		keyMap: elicitationKeyMap{
			Up:       key.NewBinding(key.WithKeys("up")),
			Down:     key.NewBinding(key.WithKeys("down")),
			Tab:      key.NewBinding(key.WithKeys("tab")),
			ShiftTab: key.NewBinding(key.WithKeys("shift+tab")),
			Enter:    key.NewBinding(key.WithKeys("enter")),
			Escape:   key.NewBinding(key.WithKeys("esc")),
			Space:    key.NewBinding(key.WithKeys("space")),
		},
		// Up/Down stay reserved for selection inside enum/boolean fields;
		// the scrollview consumes mouse wheel/scrollbar plus PgUp/PgDn/Home/End.
		scrollview: scrollview.New(scrollview.WithReserveScrollbarSpace(true)),
	}

	// If no schema fields, add a free-form text input for the response
	if len(fields) == 0 {
		ti := textinput.New()
		ti.SetStyles(styles.DialogInputStyle)
		ti.SetWidth(defaultWidth)
		ti.Prompt = ""
		ti.Placeholder = "Type your response"
		ti.CharLimit = defaultCharLimit
		ti.Focus()
		d.responseInput = ti
	}

	d.initInputs()
	return d
}

func (d *ElicitationDialog) Init() tea.Cmd {
	if d.hasFreeFormInput() || len(d.inputs) > 0 {
		return textinput.Blink
	}
	return nil
}

func (d *ElicitationDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Let the scrollview consume mouse wheel/scrollbar drag and the
	// PgUp/PgDn/Home/End keys before falling through to dialog handling.
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd
	case tea.PasteMsg:
		// Forward paste to the active text input
		if d.hasFreeFormInput() {
			var cmd tea.Cmd
			d.responseInput, cmd = d.responseInput.Update(msg)
			return d, cmd
		}
		if d.isTextInputField() {
			var cmd tea.Cmd
			d.inputs[d.currentField], cmd = d.inputs[d.currentField].Update(msg)
			return d, cmd
		}
		return d, nil
	case tea.MouseClickMsg:
		if msg.Button == tea.MouseLeft {
			return d.handleMouseClick(msg)
		}
		return d, nil
	case tea.KeyPressMsg:
		return d.handleKeyPress(msg)
	}
	return d, nil
}

func (d *ElicitationDialog) handleKeyPress(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, d.keyMap.Space) && !d.isTextInputField() && !d.hasFreeFormInput():
		// Space cycles forward through options, same as down arrow
		d.moveSelection(1)
		return d, nil
	case key.Matches(msg, d.keyMap.Escape):
		cmd := d.close(tools.ElicitationActionCancel, nil)
		return d, cmd
	case key.Matches(msg, d.keyMap.Up):
		// Up/down navigate within selection fields (enum/boolean)
		d.moveSelection(-1)
		return d, nil
	case key.Matches(msg, d.keyMap.Down):
		d.moveSelection(1)
		return d, nil
	case key.Matches(msg, d.keyMap.ShiftTab):
		d.moveFocus(-1)
		return d, nil
	case key.Matches(msg, d.keyMap.Tab):
		d.moveFocus(1)
		return d, nil
	case key.Matches(msg, d.keyMap.Enter):
		return d.submit()
	default:
		return d.updateCurrentInput(msg)
	}
}

// moveSelection moves the selection up/down within a boolean or enum field.
func (d *ElicitationDialog) moveSelection(delta int) {
	if d.currentField >= len(d.fields) {
		return
	}
	delete(d.fieldErrors, d.currentField)

	switch field := d.fields[d.currentField]; field.Type {
	case "boolean":
		// Boolean only has two options: toggle
		d.boolValues[d.currentField] = !d.boolValues[d.currentField]
	case "enum":
		n := len(field.EnumValues)
		if n == 0 {
			return
		}
		d.enumIndexes[d.currentField] = (d.enumIndexes[d.currentField] + delta + n) % n
	}
	d.ensureFocusVisible()
}

func (d *ElicitationDialog) submit() (layout.Model, tea.Cmd) {
	// Free-form response: no schema fields, just a text input
	if d.hasFreeFormInput() {
		val := strings.TrimSpace(d.responseInput.Value())
		var content map[string]any
		if val != "" {
			content = map[string]any{"response": val}
		}
		cmd := d.close(tools.ElicitationActionAccept, content)
		return d, cmd
	}

	// Schema-based form: validate all fields
	d.fieldErrors = make(map[int]string)
	content, firstErrorIdx := d.collectAndValidate()

	if firstErrorIdx >= 0 {
		d.focusField(firstErrorIdx)
		return d, nil
	}

	cmd := d.close(tools.ElicitationActionAccept, content)
	return d, cmd
}

func (d *ElicitationDialog) updateCurrentInput(msg tea.KeyPressMsg) (layout.Model, tea.Cmd) {
	if d.hasFreeFormInput() {
		var cmd tea.Cmd
		d.responseInput, cmd = d.responseInput.Update(msg)
		return d, cmd
	}
	if d.isTextInputField() {
		delete(d.fieldErrors, d.currentField)
		var cmd tea.Cmd
		d.inputs[d.currentField], cmd = d.inputs[d.currentField].Update(msg)
		return d, cmd
	}
	return d, nil
}

func (d *ElicitationDialog) moveFocus(delta int) {
	if len(d.fields) == 0 {
		return
	}
	newField := (d.currentField + delta + len(d.fields)) % len(d.fields)
	d.focusField(newField)
}

// focusField moves focus to the specified field index.
func (d *ElicitationDialog) focusField(idx int) {
	if idx < 0 || idx >= len(d.fields) {
		return
	}
	if len(d.inputs) > 0 && d.currentField < len(d.inputs) {
		d.inputs[d.currentField].Blur()
	}
	d.currentField = idx
	// Only focus text input for fields that use it
	if d.isTextInputField() {
		d.inputs[d.currentField].Focus()
	}
	d.ensureFocusVisible()
}

// ensureFocusVisible scrolls so that the focused field's active line stays
// in view. No-op before the first View() populates fieldStarts.
func (d *ElicitationDialog) ensureFocusVisible() {
	if line := d.focusLine(); line >= 0 {
		d.scrollview.EnsureLineVisible(line)
	}
}

// focusLine returns the line offset (within the scrollable body) of the
// focused field's active line — the selected option for enums/booleans, the
// input line for text fields. Returns -1 if no field is focused or layouts
// haven't been computed yet.
func (d *ElicitationDialog) focusLine() int {
	if d.currentField < 0 || d.currentField >= len(d.fieldStarts) {
		return -1
	}
	start := d.fieldStarts[d.currentField]
	switch f := d.fields[d.currentField]; f.Type {
	case "boolean":
		if d.boolValues[d.currentField] {
			return start + 1 // "Yes"
		}
		return start + 2 // "No"
	case "enum":
		idx := max(0, min(d.enumIndexes[d.currentField], len(f.EnumValues)-1))
		return start + 1 + idx
	default:
		return start + 1 // input line
	}
}

// isTextInputField returns true if the current field uses a text input (not boolean/enum).
func (d *ElicitationDialog) isTextInputField() bool {
	if d.currentField >= len(d.fields) || len(d.inputs) == 0 {
		return false
	}
	ft := d.fields[d.currentField].Type
	return ft != "boolean" && ft != "enum"
}

func (d *ElicitationDialog) close(action tools.ElicitationAction, content map[string]any) tea.Cmd {
	return CloseWithElicitationResponse(action, content)
}

// collectAndValidate validates all fields and returns the collected values.
// Returns the content map and the index of the first field with an error (-1 if valid).
func (d *ElicitationDialog) collectAndValidate() (map[string]any, int) {
	content := make(map[string]any)
	firstErrorIdx := -1

	for i, field := range d.fields {
		switch field.Type {
		case "boolean":
			content[field.Name] = d.boolValues[i]
		case "enum":
			idx := d.enumIndexes[i]
			if idx < 0 || idx >= len(field.EnumValues) {
				if field.Required {
					d.fieldErrors[i] = "Selection required"
					if firstErrorIdx < 0 {
						firstErrorIdx = i
					}
				}
				continue
			}
			content[field.Name] = field.EnumValues[idx]
		default:
			val := strings.TrimSpace(d.inputs[i].Value())
			if val == "" {
				if field.Required {
					d.fieldErrors[i] = "This field is required"
					if firstErrorIdx < 0 {
						firstErrorIdx = i
					}
				}
				continue
			}
			parsed, errMsg := d.parseAndValidateField(val, field)
			if errMsg != "" {
				d.fieldErrors[i] = errMsg
				if firstErrorIdx < 0 {
					firstErrorIdx = i
				}
				continue
			}
			content[field.Name] = parsed
		}
	}
	return content, firstErrorIdx
}

// parseAndValidateField parses and validates a field value, returning the parsed value and an error message.
func (d *ElicitationDialog) parseAndValidateField(val string, field ElicitationField) (any, string) {
	if val == "" {
		return nil, ""
	}

	switch field.Type {
	case "number":
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, "Must be a valid number"
		}
		if errMsg := validateNumberFieldWithMessage(f, field); errMsg != "" {
			return nil, errMsg
		}
		return f, ""

	case "integer":
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return nil, "Must be a whole number"
		}
		if errMsg := validateNumberFieldWithMessage(float64(n), field); errMsg != "" {
			return nil, errMsg
		}
		return n, ""

	case "enum":
		if !slices.Contains(field.EnumValues, val) {
			return nil, "Invalid selection"
		}
		return val, ""

	default: // string
		if errMsg := validateStringFieldWithMessage(val, field); errMsg != "" {
			return nil, errMsg
		}
		return val, ""
	}
}

// elicitationLayout captures the geometry computed once per render. View()
// and Position() share it so layout math lives in exactly one place.
type elicitationLayout struct {
	dialogWidth  int
	contentWidth int      // inside dialog frame
	viewport     int      // height of the scrollable region in lines
	bodyLines    []string // pre-rendered body, one entry per line
	fieldStarts  []int    // line offset of each field's label
}

// dialogHeight is the total rendered height of the dialog, including frame.
func (l elicitationLayout) dialogHeight() int { return l.viewport + elicitationOverhead }

func (d *ElicitationDialog) layout() elicitationLayout {
	dialogWidth := d.ComputeDialogWidth(70, 60, 90)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	innerWidth := max(1, contentWidth-d.scrollview.ReservedCols())

	bodyLines, fieldStarts := d.buildBody(innerWidth)
	maxViewport := max(1, min(d.Height()*80/100, 40)-elicitationOverhead)
	viewport := max(1, min(len(bodyLines), maxViewport))

	return elicitationLayout{
		dialogWidth:  dialogWidth,
		contentWidth: contentWidth,
		viewport:     viewport,
		bodyLines:    bodyLines,
		fieldStarts:  fieldStarts,
	}
}

// buildBody renders the scrollable body using the existing Content-based
// helpers and records the line offset of every field's label. Tracks line
// count incrementally to keep buildBody O(N) in the number of fields.
func (d *ElicitationDialog) buildBody(width int) (lines []string, fieldStarts []int) {
	body := NewContent(width)
	lineCount := 0

	if d.message != "" {
		msgRendered := renderMarkdownMessage(d.message, width)
		body.AddContent(msgRendered)
		lineCount += lipgloss.Height(msgRendered)
	}

	switch {
	case len(d.fields) > 0:
		body.AddSeparator()
		lineCount++ // separator adds 1 line

		fieldStarts = make([]int, len(d.fields))
		for i, field := range d.fields {
			// Record the current line count as this field's start position.
			// This avoids O(N²) by tracking line count incrementally instead
			// of calling body.Build() in the loop.
			fieldStarts[i] = lineCount

			// Render the field into a temporary Content to measure its height
			// without rebuilding the entire body.
			tempContent := NewContent(width)
			d.renderField(tempContent, i, field, width)
			fieldRendered := tempContent.Build()
			fieldHeight := lipgloss.Height(fieldRendered)

			// Add the pre-rendered field to the main body
			body.AddContent(fieldRendered)
			lineCount += fieldHeight

			if i < len(d.fields)-1 {
				body.AddSpace()
				lineCount++ // blank line separator
			}
		}

	case d.hasFreeFormInput():
		body.AddSeparator()
		d.responseInput.SetWidth(width)
		body.AddContent(d.responseInput.View())
	}

	return strings.Split(body.Build(), "\n"), fieldStarts
}

func (d *ElicitationDialog) View() string {
	l := d.layout()
	// Cache the per-field row offsets and the dialog's screen-space top row
	// so mouse-click handling in Update() can hit-test against the geometry
	// produced by this render. View() is the only place that knows the final
	// layout, so we accept the mutation as a render-cache compromise.
	d.fieldStarts = l.fieldStarts //rubocop:disable Lint/TUIViewPurity // click-zone cache consumed by Update()

	// Configure the scrollview viewport, give it the body, and scroll so the
	// focused field stays visible.
	d.scrollview.SetSize(l.contentWidth, l.viewport)
	d.scrollview.SetContent(l.bodyLines, len(l.bodyLines))
	d.ensureFocusVisible()

	// Tell the scrollview where it lives on screen (for scrollbar drag) and
	// remember the body's top row for our own mouse click hit-testing.
	row, col := CenterPosition(d.Width(), d.Height(), l.dialogWidth, l.dialogHeight())
	frameTop := styles.DialogStyle.GetBorderTopSize() + styles.DialogStyle.GetPaddingTop()
	frameLeft := styles.DialogStyle.GetBorderLeftSize() + styles.DialogStyle.GetPaddingLeft()
	d.scrollableRow = row + frameTop + elicitationHeaderLines //rubocop:disable Lint/TUIViewPurity // click-zone cache consumed by Update()
	d.scrollview.SetPosition(col+frameLeft, d.scrollableRow)

	parts := []string{
		RenderTitle(d.title, l.contentWidth, styles.DialogTitleStyle),
		RenderSeparator(l.contentWidth),
	}
	parts = append(parts, strings.Split(d.scrollview.View(), "\n")...)
	parts = append(parts, "", RenderHelpKeys(l.contentWidth, d.helpPairs()...))

	return styles.DialogStyle.Width(l.dialogWidth).Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

// helpPairs returns key/description pairs for the dialog's bottom help line,
// in left-to-right display order.
func (d *ElicitationDialog) helpPairs() []string {
	var pairs []string
	if d.hasSelectionFields() {
		pairs = append(pairs, "↑/↓", "select")
	}
	if len(d.fields) > 0 {
		pairs = append(pairs, "tab", "next field")
	}
	pairs = append(pairs, "enter", "submit", "esc", "cancel")
	if d.scrollview.NeedsScrollbar() {
		pairs = append(pairs, "pgup/pgdn", "scroll")
	}
	return pairs
}

// hasSelectionFields returns true if any field uses selection-based input (boolean or enum).
func (d *ElicitationDialog) hasSelectionFields() bool {
	for _, field := range d.fields {
		if field.Type == "boolean" || field.Type == "enum" {
			return true
		}
	}
	return false
}

func (d *ElicitationDialog) renderField(content *Content, i int, field ElicitationField, contentWidth int) {
	// Use Title if available, otherwise capitalize the property name
	label := field.Title
	if label == "" {
		label = capitalizeFirst(field.Name)
	}
	if field.Required {
		label += "*"
	}

	// Check if this field has an error
	hasError := d.fieldErrors[i] != ""
	labelStyle := styles.DialogContentStyle.Bold(true)
	if hasError {
		labelStyle = labelStyle.Foreground(styles.Error)
	}
	content.AddContent(labelStyle.Render(label))

	// Render field input based on type
	isFocused := i == d.currentField
	switch field.Type {
	case "boolean":
		d.renderBooleanField(content, i, isFocused)
	case "enum":
		d.renderEnumField(content, i, field, isFocused)
	default:
		d.inputs[i].SetWidth(contentWidth)
		content.AddContent(d.inputs[i].View())
	}

	// Show error message if present
	if hasError {
		errorStyle := styles.DialogContentStyle.Foreground(styles.Error).Italic(true)
		content.AddContent(errorStyle.Render("  ⚠ " + d.fieldErrors[i]))
	}
}

func (d *ElicitationDialog) renderBooleanField(content *Content, i int, isFocused bool) {
	selectedIdx := 1
	if d.boolValues[i] {
		selectedIdx = 0
	}
	d.renderSelectionField(content, []string{"Yes", "No"}, selectedIdx, isFocused)
}

func (d *ElicitationDialog) renderEnumField(content *Content, i int, field ElicitationField, isFocused bool) {
	d.renderSelectionField(content, field.EnumValues, d.enumIndexes[i], isFocused)
}

func (d *ElicitationDialog) renderSelectionField(content *Content, options []string, selectedIdx int, isFocused bool) {
	selectedStyle := styles.DialogContentStyle.Foreground(styles.White).Bold(true)
	unselectedStyle := styles.DialogContentStyle.Foreground(styles.TextMuted)

	for j, option := range options {
		prefix := "  ○ "
		style := unselectedStyle
		if j == selectedIdx {
			prefix = "  ● "
			if isFocused {
				prefix = "› ● "
			}
			style = selectedStyle
		}
		content.AddContent(style.Render(prefix + option))
	}
}

// capitalizeFirst returns the string with its first letter capitalized.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// handleMouseClick handles mouse click events for field focus and selection toggling.
func (d *ElicitationDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	if len(d.fieldStarts) == 0 || d.scrollableRow == 0 {
		return d, nil
	}
	relY := msg.Y - d.scrollableRow
	if relY < 0 || relY >= d.scrollview.VisibleHeight() {
		return d, nil
	}
	line := d.scrollview.ScrollOffset() + relY

	// Walk backwards: the field whose start is just at or above `line` owns it.
	// Clicks on the blank separator after a field still focus that field.
	for i := range slices.Backward(d.fieldStarts) {
		start := d.fieldStarts[i]
		if line < start {
			continue
		}
		offset := line - start
		d.focusField(i)
		delete(d.fieldErrors, i)
		switch f := d.fields[i]; f.Type {
		case "boolean":
			if offset == 1 || offset == 2 {
				d.boolValues[i] = offset == 1
			}
		case "enum":
			if offset >= 1 && offset <= len(f.EnumValues) {
				d.enumIndexes[i] = offset - 1
			}
		}
		return d, nil
	}
	return d, nil
}

func (d *ElicitationDialog) Position() (row, col int) {
	l := d.layout()
	return CenterPosition(d.Width(), d.Height(), l.dialogWidth, l.dialogHeight())
}

// --- Input initialization ---

func (d *ElicitationDialog) initInputs() {
	for i, field := range d.fields {
		d.inputs[i] = d.createInput(field, i)
	}
	// Focus the first text input field
	if d.isTextInputField() {
		d.inputs[0].Focus()
	}
}

func (d *ElicitationDialog) createInput(field ElicitationField, idx int) textinput.Model {
	ti := textinput.New()
	ti.SetStyles(styles.DialogInputStyle)
	ti.SetWidth(defaultWidth)
	ti.Prompt = "" // Remove the "> " prefix

	// Configure based on field type
	switch field.Type {
	case "boolean":
		d.boolValues[idx], _ = field.Default.(bool)
		return ti // Boolean fields don't use text input

	case "enum":
		// Initialize enum selection to first option
		d.enumIndexes[idx] = 0
		return ti // Enum fields don't use text input

	case "number", "integer":
		ti.Placeholder = cmp.Or(field.Description, "Enter a number")
		ti.CharLimit = numberCharLimit

	default: // string
		ti.Placeholder = cmp.Or(field.Description, "Enter value")
		ti.CharLimit = cmp.Or(field.MaxLength, defaultCharLimit)
	}

	// Set default value
	if field.Default != nil {
		ti.SetValue(fmt.Sprintf("%v", field.Default))
	}

	return ti
}

// renderMarkdownMessage renders a message string as markdown for display in dialogs.
// Falls back to plain text rendering if the markdown renderer fails.
func renderMarkdownMessage(message string, contentWidth int) string {
	rendered, err := markdown.NewRenderer(contentWidth).Render(message)
	if err != nil {
		return styles.DialogContentStyle.Width(contentWidth).Render(message)
	}
	return strings.TrimRight(rendered, "\n")
}
