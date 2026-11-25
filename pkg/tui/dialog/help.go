package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/core/input"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// HelpSection collects shortcuts for one context or task.
type HelpSection struct {
	Title    string
	Note     string
	Bindings []key.Binding
}

type helpDialog struct {
	readOnlyScrollDialog

	sections []HelpSection
	input    input.Text
}

// NewHelpDialog displays searchable shortcut groups, with the current context first.
func NewHelpDialog(sections []HelpSection) Dialog {
	search := input.NewText()
	search.SetStyles(styles.DialogInputStyle)
	search.Placeholder = "Search..."
	search.CharLimit = 256
	// Terminal paste is routed to Help; asynchronous clipboard replies could
	// otherwise reach the underlying text input after Help closes.
	search.KeyMap.Paste.SetEnabled(false)
	search.Focus()
	d := &helpDialog{sections: sections, input: search}
	d.readOnlyScrollDialog = newReadOnlyScrollDialog(
		readOnlyScrollDialogSize{
			widthPercent:  90,
			minWidth:      60,
			maxWidth:      132,
			heightPercent: 95,
			heightMax:     48,
		},
		d.renderContent,
	)
	d.headerLines = 4
	d.closeKey.SetKeys("esc", "enter")
	return d
}

// CursorInput returns the search input whose cursor follows the dialog stack.
func (d *helpDialog) CursorInput() input.Cursor { return &d.input }

// IsHelpDialog reports whether a dialog is the contextual Help overlay.
func IsHelpDialog(d Dialog) bool {
	_, ok := d.(*helpDialog)
	return ok
}

func (d *helpDialog) renderContent(contentWidth, viewport int) []string {
	width := max(1, contentWidth)
	inputWidth := max(1, width-lipgloss.Width(d.input.Prompt)-1)
	if d.input.Width() != inputWidth {
		d.input.SetWidth(inputWidth)
		d.input.SetCursor(d.input.Position())
	}
	lines := []string{
		styles.DialogTitleStyle.Render(ansi.Truncate("Help", width, "")),
		"",
		ansi.Truncate(d.input.View(), width, ""),
		styles.DialogSeparatorStyle.Render(strings.Repeat("─", width)),
	}
	catalog := helpCatalog{width: width, compact: viewport < 7}
	if terms := strings.Fields(d.input.Value()); len(terms) > 0 {
		d.renderMatches(&catalog, terms)
	} else {
		for _, section := range d.sections {
			catalog.section(section, nil, nil)
			firstBinding := true
			for _, binding := range section.Bindings {
				if !binding.Enabled() {
					continue
				}
				if firstBinding {
					catalog.beforeBindings()
					firstBinding = false
				}
				catalog.binding(helpBindingMatch{binding: binding})
			}
		}
	}
	return append(lines, catalog.lines...)
}

type helpCatalog struct {
	width   int
	compact bool
	lines   []string
}

func (c *helpCatalog) section(section HelpSection, title, note []helpSpan) {
	if len(c.lines) > 0 {
		c.lines = append(c.lines, "")
		if !c.compact {
			c.lines = append(c.lines, "")
		}
	}
	c.lines = append(c.lines, renderHelpText(section.Title, c.width, styles.BoldStyle.Foreground(styles.Accent).Italic(true), title)...)
	if section.Note != "" {
		c.beforeBindings()
		c.lines = append(c.lines, renderHelpText(section.Note, c.width, styles.DialogHelpStyle, note)...)
	}
}

func (c *helpCatalog) beforeBindings() {
	if !c.compact {
		c.lines = append(c.lines, "")
	}
}

func (c *helpCatalog) binding(match helpBindingMatch) {
	indent := min(2, c.width-1)
	for _, line := range renderHelpRow(c.width-indent, match) {
		c.lines = append(c.lines, strings.Repeat(" ", indent)+line)
	}
}

func (d *helpDialog) renderMatches(c *helpCatalog, terms []string) {
	matches := d.matchShortcuts(terms)
	if len(matches) == 0 {
		c.lines = renderHelpText("No matching shortcuts.", c.width, styles.DialogHelpStyle, nil)
		return
	}
	prevSection := -1
	for _, match := range matches {
		if match.section != prevSection {
			c.section(d.sections[match.section], match.title, match.note)
			if len(match.rows) > 0 {
				c.beforeBindings()
			}
			prevSection = match.section
		}
		for _, row := range match.rows {
			c.binding(row)
		}
	}
}

func renderHelpRow(width int, match helpBindingMatch) []string {
	const gap = 2
	label := strings.Join(match.binding.Keys(), ", ")
	description := match.binding.Help().Desc
	keyStyle := styles.HighlightWhiteStyle
	descStyle := styles.SecondaryStyle
	if width < 8 {
		keys := renderHelpText(label, width, keyStyle, match.keys)
		return append(keys, renderHelpText(description, width, descStyle, match.desc)...)
	}
	keyWidth := min(19, width/3)
	keys := renderHelpText(label, keyWidth, keyStyle, match.keys)
	descriptions := renderHelpText(description, width-keyWidth-gap, descStyle, match.desc)
	lines := make([]string, max(len(keys), len(descriptions)))
	for i := range lines {
		padding := keyWidth + gap
		if i < len(keys) {
			lines[i] = keys[i]
			padding = max(0, padding-lipgloss.Width(keys[i]))
		}
		if i < len(descriptions) {
			lines[i] += strings.Repeat(" ", padding) + descriptions[i]
		}
	}
	return lines
}

// Wrap the plain text before styling; a scrolled-in line needs all its own styles.
func renderHelpText(text string, width int, style lipgloss.Style, spans []helpSpan) []string {
	lines := strings.Split(ansi.Wrap(text, max(1, width), ""), "\n")
	offset := 0
	for i, line := range lines {
		if len(spans) > 0 && line != "" {
			if start := strings.Index(text[offset:], line); start >= 0 {
				offset += start
				lines[i] = renderHelpHighlights(line, offset, style, spans)
				offset += len(line)
				continue
			}
		}
		lines[i] = style.Render(line)
	}
	return lines
}

func renderHelpHighlights(line string, offset int, style lipgloss.Style, spans []helpSpan) string {
	var out strings.Builder
	highlight := style.Foreground(styles.Highlight).Underline(true)
	pos := 0
	for _, span := range spans {
		start, end := max(pos, span.start-offset), min(len(line), span.end-offset)
		if start >= end {
			continue
		}
		if pos < start {
			out.WriteString(style.Render(line[pos:start]))
		}
		out.WriteString(highlight.Render(line[start:end]))
		pos = end
	}
	if pos < len(line) {
		out.WriteString(style.Render(line[pos:]))
	}
	return out.String()
}

func (d *helpDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		// Keep the read-only scroll controls except j/k, which are filter text.
		if msg.Text != "" {
			cmd := d.updateInput(msg)
			return d, cmd
		}
		if handled, cmd := d.scrollview.Update(msg); handled {
			return d, cmd
		}
		if key.Matches(msg, d.closeKey) {
			return d, closeDialogCmd()
		}
		cmd := d.updateInput(msg)
		return d, cmd
	case tea.PasteMsg:
		cmd := d.updateInput(msg)
		return d, cmd
	case messages.ThemeChangedMsg:
		d.input.SetStyles(styles.DialogInputStyle)
	}
	_, cmd := d.readOnlyScrollDialog.Update(msg)
	return d, cmd
}

func (d *helpDialog) updateInput(msg tea.Msg) tea.Cmd {
	before := d.input.Value()
	var cmd tea.Cmd
	d.input, cmd = d.input.Update(msg)
	if d.input.Value() != before {
		d.scrollview.ScrollToTop()
	}
	return cmd
}

// ContextHelp describes common modal controls. Dialogs with specialized
// controls continue to show their own hints inside the underlying dialog.
func ContextHelp(d Dialog) HelpSection {
	binding := func(b key.Binding, description string) key.Binding {
		b.SetHelp("", description)
		return b
	}
	picker := func(title string, keys pickerKeyMap, action string) HelpSection {
		return HelpSection{Title: title, Note: "Type to filter; Ctrl+h or Backspace deletes text.", Bindings: []key.Binding{
			binding(keys.Up, "Move up"), binding(keys.Down, "Move down"), binding(keys.Enter, action), binding(keys.Escape, "Close"),
		}}
	}
	switch d := d.(type) {
	case *commandPaletteDialog:
		return picker("Command palette", d.keyMap, "Run selected command")
	case *modelPickerDialog:
		section := picker("Model picker", d.keyMap, "Choose selected model")
		section.Bindings = append(section.Bindings, d.refresh)
		return section
	case *themePickerDialog:
		return picker("Theme picker", d.keyMap, "Apply selected theme")
	case *filePickerDialog:
		return picker("File picker", d.keyMap, "Open selected file or directory")
	case *exitConfirmationDialog:
		return HelpSection{Title: "Exit confirmation", Bindings: []key.Binding{
			binding(d.keyMap.Yes, "Confirm exit"), binding(d.keyMap.No, "Keep session open"), binding(d.keyMap.Esc, "Cancel"),
		}}
	default:
		return HelpSection{Title: "Open dialog", Note: "Use the controls shown in the underlying dialog. Chat shortcuts do not replace dialog keys."}
	}
}
