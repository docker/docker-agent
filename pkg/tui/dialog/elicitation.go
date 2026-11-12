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
	// EnumTitles and EnumDescriptions parallel EnumValues for titled choices
	// (oneOf / items.anyOf); EnumTitles are shown instead of the values.
	EnumTitles, EnumDescriptions []string
	Default                      any
	MinLength, MaxLength         int
	MinItems, MaxItems           int
	Format, Pattern              string
	Minimum, Maximum             float64
	HasMinimum, HasMaximum       bool
}

// optionLabels returns what the dialog shows for each choice.
func (f ElicitationField) optionLabels() []string {
	if len(f.EnumTitles) == len(f.EnumValues) {
		return f.EnumTitles
	}
	return f.EnumValues
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
	elicitationID string
	fields        []ElicitationField
	inputs        []textinput.Model
	boolValues    map[int]bool
	enumIndexes   map[int]int // selected index for enum fields
	multiChecked  map[int][]bool
	multiCursor   map[int]int // focused option of multiselect fields
	currentField  int
	keyMap        elicitationKeyMap
	fieldErrors   map[int]string  // validation error messages per field
	responseInput textinput.Model // free-form text input used when len(fields) == 0

	scrollview *scrollview.Model
	// fieldStarts[i] is the line offset of field i's label inside the
	// scrollable body. Populated by View() / Position().
	fieldStarts []int
	// fieldGeoms[i] describes field i's wrapped row layout (label height and
	// per-option row ranges) relative to fieldStarts[i]. Populated together
	// with fieldStarts by View().
	fieldGeoms []fieldGeometry
	// scrollableRow is the absolute screen row of the first scrollable line.
	scrollableRow int
	// reanchorFocus asks the next View() to scroll the focused control back
	// into view. Set on real resizes: rewrapping at the new width can move
	// the focused rows far from the current scroll offset, and only View()
	// knows the fresh geometry. Consumed one-shot so ordinary renders never
	// override the user's scroll position.
	reanchorFocus bool
}

// fieldGeometry records the row layout of a single rendered field, relative
// to the field's first line. Long labels and options wrap over several rows,
// so focus and mouse hit-testing consult these offsets instead of assuming
// one row per label/option.
type fieldGeometry struct {
	labelHeight   int   // rows used by the wrapped label
	optionStarts  []int // first row of each enum/boolean option, relative to the field start
	optionHeights []int // rows used by each option, parallel to optionStarts
}

// optionAt maps a row offset (relative to the field's first line) to the
// index of the enum/boolean option rendered on that row, or -1 when the
// offset falls outside every option (label rows, error line, trailing blank).
func (g fieldGeometry) optionAt(offset int) int {
	for j, start := range g.optionStarts {
		if offset >= start && offset < start+g.optionHeights[j] {
			return j
		}
	}
	return -1
}

type elicitationKeyMap struct {
	Up, Down, Tab, ShiftTab, Enter, Escape, Space key.Binding
}

// hasFreeFormInput returns true when no schema fields exist and the dialog
// shows a single free-form text input instead.
func (d *ElicitationDialog) hasFreeFormInput() bool {
	return len(d.fields) == 0
}

// firstElicitationID extracts the (at most one meaningful) elicitation ID
// from a variadic parameter. Shared by the elicitation dialog constructors
// in this package, whose elicitationID parameter is variadic rather than a
// required positional string purely so pre-#3584 3-arg call sites keep
// compiling unchanged (mirrors pkg/runtime.firstElicitationID).
func firstElicitationID(elicitationID []string) string {
	if len(elicitationID) == 0 {
		return ""
	}
	return elicitationID[0]
}

// NewElicitationDialog creates a new elicitation dialog. elicitationID is
// variadic purely so pre-existing 3-arg callers keep compiling unchanged
// (see firstElicitationID for the same #3584 precedent); at most the first
// value is meaningful.
func NewElicitationDialog(message string, schema any, meta map[string]any, elicitationID ...string) Dialog {
	fields := parseElicitationSchema(schema)
	id := firstElicitationID(elicitationID)

	// Determine dialog title from meta, defaulting to "Question"
	title := "Question"
	if meta != nil {
		if t, ok := meta["cagent/title"].(string); ok && t != "" {
			title = t
		}
	}

	d := &ElicitationDialog{
		title:         title,
		message:       message,
		elicitationID: id,
		fields:        fields,
		inputs:        make([]textinput.Model, len(fields)),
		boolValues:    make(map[int]bool),
		enumIndexes:   make(map[int]int),
		multiChecked:  make(map[int][]bool),
		multiCursor:   make(map[int]int),
		fieldErrors:   make(map[int]string),
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
		// A real resize rewraps the body, which can strand the focused
		// control far from the current scroll offset; have the next View()
		// reanchor it once fresh geometry exists. Skip the initial sizing at
		// open so the dialog still opens scrolled to the top.
		if d.Width() > 0 && (msg.Width != d.Width() || msg.Height != d.Height()) {
			d.reanchorFocus = true
		}
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
			d.ensureFocusVisible()
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
		// Space toggles a multiselect option, and cycles forward through
		// other options, same as down arrow
		if d.fields[d.currentField].Type == "multiselect" {
			d.toggleOption(d.currentField, d.multiCursor[d.currentField])
			return d, nil
		}
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
	case "multiselect":
		n := len(field.EnumValues)
		d.multiCursor[d.currentField] = (d.multiCursor[d.currentField] + delta + n) % n
	}
	d.ensureFocusVisible()
}

// toggleOption checks or unchecks option j of multiselect field i.
func (d *ElicitationDialog) toggleOption(i, j int) {
	checked := d.multiChecked[i]
	if j < 0 || j >= len(checked) {
		return
	}
	delete(d.fieldErrors, i)
	checked[j] = !checked[j]
	d.multiCursor[i] = j
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
		// If the field was below the fold (e.g. the dialog opened scrolled to
		// the top with a tall message), reveal it as soon as the user starts
		// typing so they can see what they're entering.
		d.ensureFocusVisible()
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

// ensureFocusVisible scrolls so that the focused field's active rows stay
// in view. No-op before the first View() populates fieldStarts.
func (d *ElicitationDialog) ensureFocusVisible() {
	if start, end := d.focusRange(); start >= 0 {
		d.scrollview.EnsureRangeVisible(start, end)
	}
}

// focusRange returns the first and last line offsets (within the scrollable
// body) of the focused field's active rows — the selected option for
// enums/booleans, the input line for text fields. Wrapping can spread an
// option over several rows, hence a range. Returns (-1, -1) if no field is
// focused or layouts haven't been computed yet.
func (d *ElicitationDialog) focusRange() (start, end int) {
	if d.currentField < 0 || d.currentField >= len(d.fieldStarts) || len(d.fieldGeoms) != len(d.fieldStarts) {
		return -1, -1
	}
	fieldStart := d.fieldStarts[d.currentField]
	geom := d.fieldGeoms[d.currentField]

	idx := -1
	switch f := d.fields[d.currentField]; f.Type {
	case "boolean":
		idx = 1 // "No"
		if d.boolValues[d.currentField] {
			idx = 0 // "Yes"
		}
	case "enum":
		idx = max(0, min(d.enumIndexes[d.currentField], len(f.EnumValues)-1))
	case "multiselect":
		idx = d.multiCursor[d.currentField]
	}
	if idx < 0 || idx >= len(geom.optionStarts) {
		line := fieldStart + geom.labelHeight // input line
		return line, line
	}
	start = fieldStart + geom.optionStarts[idx]
	return start, start + geom.optionHeights[idx] - 1
}

// isTextInputField returns true if the current field uses a text input (not boolean/enum).
func (d *ElicitationDialog) isTextInputField() bool {
	if d.currentField >= len(d.fields) || len(d.inputs) == 0 {
		return false
	}
	ft := d.fields[d.currentField].Type
	return ft != "boolean" && ft != "enum" && ft != "multiselect"
}

func (d *ElicitationDialog) close(action tools.ElicitationAction, content map[string]any) tea.Cmd {
	return CloseWithElicitationResponse(action, content, d.elicitationID)
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
		case "multiselect":
			selected := []any{}
			for j, checked := range d.multiChecked[i] {
				if checked {
					selected = append(selected, field.EnumValues[j])
				}
			}
			if errMsg := validateSelectionCount(len(selected), field); errMsg != "" {
				d.fieldErrors[i] = errMsg
				if firstErrorIdx < 0 {
					firstErrorIdx = i
				}
				continue
			}
			content[field.Name] = selected
		default:
			raw := d.inputs[i].Value()
			val := strings.TrimSpace(raw)
			if val == "" {
				if field.Required {
					d.fieldErrors[i] = "This field is required"
					if firstErrorIdx < 0 {
						firstErrorIdx = i
					}
				}
				continue
			}
			// Secret fields (e.g. a sudo password) may legitimately contain
			// leading/trailing whitespace; store the raw value. The trimmed
			// copy above is only used for the required/empty check.
			if field.Format == "password" {
				val = raw
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
	contentWidth int             // inside dialog frame
	viewport     int             // height of the scrollable region in lines
	bodyLines    []string        // pre-rendered body, one entry per line
	fieldStarts  []int           // line offset of each field's label
	fieldGeoms   []fieldGeometry // wrapped row layout per field, relative to fieldStarts
}

// dialogHeight is the total rendered height of the dialog, including frame.
func (l elicitationLayout) dialogHeight() int { return l.viewport + elicitationOverhead }

func (d *ElicitationDialog) layout() elicitationLayout {
	dialogWidth := d.ComputeDialogWidth(70, 60, 90)
	contentWidth := d.ContentWidth(dialogWidth, 2)
	innerWidth := max(1, contentWidth-d.scrollview.ReservedCols())

	bodyLines, fieldStarts, fieldGeoms := d.buildBody(innerWidth)
	maxViewport := max(1, min(d.Height()*80/100, 40)-elicitationOverhead)
	viewport := max(1, min(len(bodyLines), maxViewport))

	return elicitationLayout{
		dialogWidth:  dialogWidth,
		contentWidth: contentWidth,
		viewport:     viewport,
		bodyLines:    bodyLines,
		fieldStarts:  fieldStarts,
		fieldGeoms:   fieldGeoms,
	}
}

// buildBody renders the scrollable body using the existing Content-based
// helpers and records the line offset of every field's label plus each
// field's wrapped row geometry. Tracks line count incrementally to keep
// buildBody O(N) in the number of fields.
func (d *ElicitationDialog) buildBody(width int) (lines []string, fieldStarts []int, fieldGeoms []fieldGeometry) {
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
		fieldGeoms = make([]fieldGeometry, len(d.fields))
		for i, field := range d.fields {
			// Record the current line count as this field's start position.
			// This avoids O(N²) by tracking line count incrementally instead
			// of calling body.Build() in the loop.
			fieldStarts[i] = lineCount

			// Render the field into a temporary Content to measure its height
			// without rebuilding the entire body.
			tempContent := NewContent(width)
			fieldGeoms[i] = d.renderField(tempContent, i, field, width)
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

	return strings.Split(body.Build(), "\n"), fieldStarts, fieldGeoms
}

func (d *ElicitationDialog) View() string {
	l := d.layout()
	// Cache the per-field row offsets and the dialog's screen-space top row
	// so mouse-click handling in Update() can hit-test against the geometry
	// produced by this render. View() is the only place that knows the final
	// layout, so we accept the mutation as a render-cache compromise.
	d.fieldStarts = l.fieldStarts //rubocop:disable Lint/TUIViewPurity // click-zone cache consumed by Update()
	d.fieldGeoms = l.fieldGeoms   //rubocop:disable Lint/TUIViewPurity // click-zone cache consumed by Update()

	// Configure the scrollview viewport and give it the body. Scroll position
	// is intentionally not adjusted here: the dialog opens scrolled to the top
	// (so the user can read the full question/message from the start), and
	// only changes when the user interacts (focus moves, selection changes,
	// or scroll keys/wheel). Auto-scrolling on every render would prevent the
	// user from scrolling back up to see the question header above the
	// initially focused option/field.
	d.scrollview.SetSize(l.contentWidth, l.viewport)
	d.scrollview.SetContent(l.bodyLines, len(l.bodyLines))

	// One-shot exception to the no-auto-scroll rule above: right after a
	// resize the rewrapped geometry can leave the focused control outside
	// the old scroll offset, so reanchor it now that fieldStarts/fieldGeoms
	// and the scrollview dimensions are fresh. Free-form dialogs have no
	// field rows, so this is a no-op for them.
	if d.reanchorFocus {
		d.reanchorFocus = false //rubocop:disable Lint/TUIViewPurity // one-shot resize flag consumed by the first render at the new size
		d.ensureFocusVisible()
	}

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
	if slices.ContainsFunc(d.fields, func(f ElicitationField) bool { return f.Type == "multiselect" }) {
		pairs = append(pairs, "space", "toggle")
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

// hasSelectionFields returns true if any field uses selection-based input (boolean, enum or multiselect).
func (d *ElicitationDialog) hasSelectionFields() bool {
	for _, field := range d.fields {
		if field.Type == "boolean" || field.Type == "enum" || field.Type == "multiselect" {
			return true
		}
	}
	return false
}

func (d *ElicitationDialog) renderField(content *Content, i int, field ElicitationField, contentWidth int) fieldGeometry {
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
	// Wrap long agent-supplied labels to the content width; anything wider
	// would be silently clipped by the scrollview.
	renderedLabel := labelStyle.Width(contentWidth).Render(label)
	content.AddContent(renderedLabel)
	geom := fieldGeometry{labelHeight: lipgloss.Height(renderedLabel)}

	// Render field input based on type
	isFocused := i == d.currentField
	switch field.Type {
	case "boolean":
		d.renderBooleanField(content, &geom, i, isFocused, contentWidth)
	case "enum":
		d.renderEnumField(content, &geom, i, field, isFocused, contentWidth)
	case "multiselect":
		d.renderMultiSelectField(content, &geom, i, field, isFocused, contentWidth)
	default:
		d.inputs[i].SetWidth(contentWidth)
		content.AddContent(d.inputs[i].View())
	}

	// Show error message if present
	if hasError {
		errorStyle := styles.DialogContentStyle.Foreground(styles.Error).Italic(true)
		content.AddContent(errorStyle.Render("  ⚠ " + d.fieldErrors[i]))
	}
	return geom
}

func (d *ElicitationDialog) renderBooleanField(content *Content, geom *fieldGeometry, i int, isFocused bool, contentWidth int) {
	selectedIdx := 1
	if d.boolValues[i] {
		selectedIdx = 0
	}
	d.renderSelectionField(content, geom, []string{"Yes", "No"}, nil, selectedIdx, isFocused, contentWidth)
}

func (d *ElicitationDialog) renderEnumField(content *Content, geom *fieldGeometry, i int, field ElicitationField, isFocused bool, contentWidth int) {
	d.renderSelectionField(content, geom, field.optionLabels(), field.EnumDescriptions, d.enumIndexes[i], isFocused, contentWidth)
}

func (d *ElicitationDialog) renderSelectionField(content *Content, geom *fieldGeometry, options, descriptions []string, selectedIdx int, isFocused bool, contentWidth int) {
	d.renderOptions(content, geom, options, descriptions, contentWidth, func(j int) (string, bool) {
		if j != selectedIdx {
			return "  ○ ", false
		}
		if isFocused {
			return "› ● ", true
		}
		return "  ● ", true
	})
}

func (d *ElicitationDialog) renderMultiSelectField(content *Content, geom *fieldGeometry, i int, field ElicitationField, isFocused bool, contentWidth int) {
	checked, cursor := d.multiChecked[i], d.multiCursor[i]
	d.renderOptions(content, geom, field.optionLabels(), field.EnumDescriptions, contentWidth, func(j int) (string, bool) {
		lead, box := "  ", "[ ] "
		if isFocused && j == cursor {
			lead = "› "
		}
		if checked[j] {
			box = "[x] "
		}
		return lead + box, checked[j]
	})
}

// renderOptions renders one row per option, with its description below in a
// muted style; marker gives each option's prefix and whether it stands out.
func (d *ElicitationDialog) renderOptions(content *Content, geom *fieldGeometry, options, descriptions []string, contentWidth int, marker func(j int) (prefix string, highlighted bool)) {
	selectedStyle := styles.DialogContentStyle.Foreground(styles.TextBright).Bold(true)
	unselectedStyle := styles.DialogContentStyle.Foreground(styles.TextMuted)
	descriptionStyle := styles.DialogContentStyle.Foreground(styles.TextMuted).Italic(true)

	row := geom.labelHeight
	for j, option := range options {
		prefix, highlighted := marker(j)
		style := unselectedStyle
		if highlighted {
			style = selectedStyle
		}
		// All prefixes of a field are equally wide, so option heights don't
		// change with focus/selection and wrapped rows hang-indent under the
		// option text.
		textWidth := max(1, contentWidth-lipgloss.Width(prefix))
		text := style.Width(textWidth).Render(option)
		if j < len(descriptions) && descriptions[j] != "" {
			text = lipgloss.JoinVertical(lipgloss.Left, text, descriptionStyle.Width(textWidth).Render(descriptions[j]))
		}
		// Wrap long agent-supplied options instead of letting the scrollview
		// clip them; JoinHorizontal keeps continuation rows aligned under the
		// first row's text column.
		rendered := lipgloss.JoinHorizontal(lipgloss.Top, style.Render(prefix), text)
		content.AddContent(rendered)
		height := lipgloss.Height(rendered)
		geom.optionStarts = append(geom.optionStarts, row)
		geom.optionHeights = append(geom.optionHeights, height)
		row += height
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
	if len(d.fieldStarts) == 0 || len(d.fieldGeoms) != len(d.fieldStarts) || d.scrollableRow == 0 {
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
		d.focusField(i)
		delete(d.fieldErrors, i)
		// Any wrapped row of an option counts as a click on that option.
		opt := d.fieldGeoms[i].optionAt(line - start)
		if opt < 0 {
			return d, nil
		}
		switch f := d.fields[i]; f.Type {
		case "boolean":
			d.boolValues[i] = opt == 0
		case "enum":
			if opt < len(f.EnumValues) {
				d.enumIndexes[i] = opt
			}
		case "multiselect":
			d.toggleOption(i, opt)
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

	case "multiselect":
		defaults, _ := field.Default.([]any)
		checked := make([]bool, len(field.EnumValues))
		for j, value := range field.EnumValues {
			checked[j] = slices.Contains(defaults, any(value))
		}
		d.multiChecked[idx] = checked
		return ti

	case "number", "integer":
		ti.Placeholder = cmp.Or(field.Description, "Enter a number")
		ti.CharLimit = numberCharLimit

	default: // string
		ti.Placeholder = cmp.Or(field.Description, "Enter value")
		ti.CharLimit = cmp.Or(field.MaxLength, defaultCharLimit)
		// Mask secret fields (JSON Schema "format": "password"), e.g. the sudo
		// password prompt, so the value is never echoed to the screen.
		if field.Format == "password" {
			ti.EchoMode = textinput.EchoPassword
		}
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
	// No copy icon: dialogs do not hit-test clicks on code blocks.
	rendered, err := markdown.NewRendererWithoutCopyIcon(contentWidth).Render(message)
	if err != nil {
		return styles.DialogContentStyle.Width(contentWidth).Render(message)
	}
	return strings.TrimRight(rendered, "\n")
}
