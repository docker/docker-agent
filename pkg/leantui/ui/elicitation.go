package ui

import (
	"cmp"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/elicitform"
)

type ElicitationKind int

const (
	ElicitationForm ElicitationKind = iota
	ElicitationURL
	ElicitationOAuth
)

// ElicitationResult is what a key press asks the caller to do. An empty
// Action keeps the prompt open.
type ElicitationResult struct {
	Action  tools.ElicitationAction
	Content map[string]any
	OpenURL bool
}

func (r ElicitationResult) Done() bool { return r.Action != "" }

// ElicitationModel is an MCP server prompt shown in place of the editor: a
// schema form, a free-form question, a URL to visit, or an OAuth consent.
type ElicitationModel struct {
	Kind    ElicitationKind
	Title   string
	Message string
	URL     string

	fields  []elicitform.Field
	answers []elicitform.Answer
	inputs  []*Editor // text fields only; free-form uses inputs[0] with no fields
	options []int     // multiselect option cursor
	errs    map[int]string
	focus   int
}

func NewFormElicitation(title, message string, schema any) *ElicitationModel {
	fields := elicitform.ParseSchema(schema)
	e := &ElicitationModel{
		Kind:    ElicitationForm,
		Title:   cmp.Or(title, "Question"),
		Message: message,
		fields:  fields,
		answers: make([]elicitform.Answer, len(fields)),
		inputs:  make([]*Editor, max(len(fields), 1)),
		options: make([]int, len(fields)),
		errs:    map[int]string{},
	}
	if len(fields) == 0 {
		e.inputs[0] = NewEditor("Type your response")
		return e
	}
	for i, f := range fields {
		e.answers[i] = elicitform.DefaultAnswer(f)
		if !f.IsText() {
			continue
		}
		placeholder := "Enter value"
		if f.Type == elicitform.TypeNumber || f.Type == elicitform.TypeInteger {
			placeholder = "Enter a number"
		}
		ed := NewEditor(cmp.Or(f.Description, placeholder))
		if f.IsSecret() {
			ed.SetMask('•')
		}
		ed.SetText(e.answers[i].Text)
		e.inputs[i] = ed
	}
	return e
}

func NewURLElicitation(message, url string) *ElicitationModel {
	return &ElicitationModel{Kind: ElicitationURL, Title: "MCP server request", Message: message, URL: url}
}

func NewOAuthElicitation(serverURL string) *ElicitationModel {
	return &ElicitationModel{Kind: ElicitationOAuth, Title: "OAuth authorization required", URL: serverURL}
}

// HandleKey applies k to the prompt.
func (e *ElicitationModel) HandleKey(k Key) ElicitationResult {
	if k.Typ == KeyEsc {
		if e.Kind == ElicitationOAuth {
			return ElicitationResult{Action: tools.ElicitationActionDecline}
		}
		return ElicitationResult{Action: tools.ElicitationActionCancel}
	}
	if e.Kind != ElicitationForm {
		return e.handleConfirmKey(k)
	}
	if k.Typ == KeyEnter {
		return e.submit()
	}
	if len(e.fields) == 0 {
		editLine(e.inputs[0], k)
		return ElicitationResult{}
	}

	switch k.Typ {
	case KeyTab:
		e.moveFocus(1)
	case KeyShiftTab:
		e.moveFocus(-1)
	case KeyUp:
		e.moveVertical(-1)
	case KeyDown:
		e.moveVertical(1)
	default:
		if ed := e.inputs[e.focus]; ed != nil {
			if editLine(ed, k) {
				delete(e.errs, e.focus)
			}
		} else if k.Typ == KeyRune && len(k.Runes) == 1 && k.Runes[0] == ' ' {
			e.toggle()
		}
	}
	return ElicitationResult{}
}

func (e *ElicitationModel) handleConfirmKey(k Key) ElicitationResult {
	if k.Typ == KeyEnter {
		return ElicitationResult{Action: tools.ElicitationActionAccept}
	}
	if k.Typ != KeyRune || len(k.Runes) != 1 {
		return ElicitationResult{}
	}
	switch k.Runes[0] {
	case 'y', 'Y':
		return ElicitationResult{Action: tools.ElicitationActionAccept}
	case 'n', 'N':
		return ElicitationResult{Action: tools.ElicitationActionDecline}
	case 'o', 'O':
		return ElicitationResult{OpenURL: e.Kind == ElicitationURL && e.URL != ""}
	}
	return ElicitationResult{}
}

func (e *ElicitationModel) submit() ElicitationResult {
	if len(e.fields) == 0 {
		return ElicitationResult{Action: tools.ElicitationActionAccept, Content: elicitform.FreeForm(e.inputs[0].Text())}
	}
	content, errs, first := elicitform.Collect(e.fields, func(i int) elicitform.Answer {
		a := e.answers[i]
		if e.inputs[i] != nil {
			a.Text = e.inputs[i].Text()
		}
		return a
	})
	e.errs = errs
	if first >= 0 {
		e.focus = first
		return ElicitationResult{}
	}
	return ElicitationResult{Action: tools.ElicitationActionAccept, Content: content}
}

func (e *ElicitationModel) moveFocus(delta int) {
	n := len(e.fields)
	e.focus = (e.focus + delta + n) % n
}

// moveVertical picks among a choice field's options; text fields hand the
// arrow to field navigation since they are single-line.
func (e *ElicitationModel) moveVertical(delta int) {
	f := e.fields[e.focus]
	switch f.Type {
	case elicitform.TypeBoolean:
		e.toggle()
	case elicitform.TypeEnum:
		if n := len(f.EnumValues); n > 0 {
			e.answers[e.focus].Choice = (e.answers[e.focus].Choice + delta + n) % n
			delete(e.errs, e.focus)
		}
	case elicitform.TypeMultiSelect:
		if n := len(f.EnumValues); n > 0 {
			e.options[e.focus] = (e.options[e.focus] + delta + n) % n
		}
	default:
		e.moveFocus(delta)
	}
}

func (e *ElicitationModel) toggle() {
	a := &e.answers[e.focus]
	switch e.fields[e.focus].Type {
	case elicitform.TypeBoolean:
		a.Bool = !a.Bool
	case elicitform.TypeEnum:
		e.moveVertical(1)
	case elicitform.TypeMultiSelect:
		if j := e.options[e.focus]; j < len(a.Checked) {
			a.Checked[j] = !a.Checked[j]
		}
	}
	delete(e.errs, e.focus)
}

// editLine applies single-line editing keys and reports whether k was one.
func editLine(ed *Editor, k Key) bool {
	switch k.Typ {
	case KeyRune, KeyPaste:
		ed.Insert([]rune(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(string(k.Runes))))
	case KeyBackspace:
		ed.Backspace()
	case KeyDelete:
		ed.DeleteForward()
	case KeyLeft:
		ed.MoveLeft()
	case KeyRight:
		ed.MoveRight()
	case KeyWordLeft:
		ed.MoveWordLeft()
	case KeyWordRight:
		ed.MoveWordRight()
	case KeyHome:
		ed.MoveLineStart()
	case KeyEnd:
		ed.MoveLineEnd()
	case KeyCtrlU:
		ed.DeleteToLineStart()
	case KeyCtrlK:
		ed.DeleteToLineEnd()
	case KeyCtrlW:
		ed.DeleteWordBack()
	default:
		return false
	}
	return true
}

// Render lays out the prompt and returns the hardware cursor position
// within the returned lines.
func (e *ElicitationModel) Render(width int) (lines []string, cursorRow, cursorCol int) {
	cursorRow = -1
	add := func(s string) { lines = append(lines, Truncate(s, width)) }

	add(StWarning().Render("● " + e.Title))
	if strings.TrimSpace(e.Message) != "" {
		lines = append(lines, RenderAssistantLines(e.Message, width)...)
	}

	switch e.Kind {
	case ElicitationURL:
		if e.URL != "" {
			add(StMuted().Render("Visit: ") + StAccent().Render(e.URL))
		}
		add(StMuted().Render("[y] done   [n] decline   [o] open in browser   [esc] cancel"))
	case ElicitationOAuth:
		lines = append(lines, RenderNoticeLines("", e.URL+" requires OAuth. Your browser will open to complete authorization.", width, StPrimary())...)
		add(StMuted().Render("[y] authorize   [n] decline"))
	default:
		if len(e.fields) == 0 {
			editorLines, row, col := e.inputs[0].Layout(width)
			cursorRow, cursorCol = len(lines)+row, col
			lines = append(lines, editorLines...)
			add(StMuted().Render("Enter submit · Esc cancel"))
			break
		}
		for i := range e.fields {
			start := len(lines)
			fieldLines, row, col := e.renderField(i, width)
			lines = append(lines, fieldLines...)
			if i == e.focus && row >= 0 {
				cursorRow, cursorCol = start+row, col
			}
		}
		add(StMuted().Render(e.helpText()))
	}

	if cursorRow < 0 {
		cursorRow = len(lines) - 1
		cursorCol = min(DisplayWidth(lines[cursorRow]), max(width-1, 0))
	}
	return lines, cursorRow, cursorCol
}

// renderField returns the field's rows and, for focused text fields, the
// cursor row within them (-1 otherwise).
func (e *ElicitationModel) renderField(i, width int) (lines []string, cursorRow, cursorCol int) {
	const indent = "  "
	f := e.fields[i]
	focused := i == e.focus
	cursorRow = -1

	label := cmp.Or(f.Title, f.Name)
	if f.Required {
		label += " *"
	}
	marker, labelStyle := "  ", StSecondary()
	if focused {
		marker, labelStyle = "› ", StBold()
	}
	lines = append(lines, Truncate(labelStyle.Render(marker+label), width))
	if f.Description != "" && !f.IsText() {
		lines = append(lines, RenderNoticeLines(indent, f.Description, width, StMuted())...)
	}

	a := e.answers[i]
	switch f.Type {
	case elicitform.TypeBoolean:
		yes, no := "○ Yes", "● No"
		if a.Bool {
			yes, no = "● Yes", "○ No"
		}
		lines = append(lines, Truncate(indent+highlight(focused).Render(yes+"   "+no), width))
	case elicitform.TypeEnum:
		for j, opt := range f.OptionLabels() {
			bullet := "○ "
			if j == a.Choice {
				bullet = "● "
			}
			lines = append(lines, Truncate(indent+highlight(focused && j == a.Choice).Render(bullet+opt)+optionDescription(f, j), width))
		}
	case elicitform.TypeMultiSelect:
		for j, opt := range f.OptionLabels() {
			box := "[ ] "
			if j < len(a.Checked) && a.Checked[j] {
				box = "[x] "
			}
			lines = append(lines, Truncate(indent+highlight(focused && j == e.options[i]).Render(box+opt)+optionDescription(f, j), width))
		}
	default:
		editorLines, row, col := e.inputs[i].Layout(width - len(indent))
		if focused {
			cursorRow, cursorCol = len(lines)+row, col+len(indent)
		}
		for _, l := range editorLines {
			lines = append(lines, indent+l)
		}
	}

	if msg := e.errs[i]; msg != "" {
		lines = append(lines, Truncate(indent+StError().Render("✗ "+msg), width))
	}
	return lines, cursorRow, cursorCol
}

func (e *ElicitationModel) helpText() string {
	parts := []string{}
	if len(e.fields) > 1 {
		parts = append(parts, "Tab next field")
	}
	if f := e.fields[e.focus]; !f.IsText() {
		parts = append(parts, "↑/↓ choose")
		if f.Type != elicitform.TypeEnum {
			parts = append(parts, "Space toggle")
		}
	}
	return strings.Join(append(parts, "Enter submit", "Esc cancel"), " · ")
}

func highlight(highlighted bool) lipgloss.Style {
	if highlighted {
		return StAccent()
	}
	return StPrimary()
}

func optionDescription(f elicitform.Field, j int) string {
	if j >= len(f.EnumDescriptions) || f.EnumDescriptions[j] == "" {
		return ""
	}
	return StMuted().Render(" — " + f.EnumDescriptions[j])
}
