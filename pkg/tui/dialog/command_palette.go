package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// commandPaletteDialog implements Dialog for the command palette.
// It uses pickerCore for the shared filter/scroll/select skeleton and only
// adds the bits that are specific to running commands.
type commandPaletteDialog struct {
	pickerCore

	categories []commands.Category
	filtered   []commands.Item
}

// commandPaletteLayout is the layout used by the command palette.
//
// ListOverhead = title(1) + space(1) + input(1) + separator(1) + space(1) +
//
//	help(1) + borders/padding(2) = 8
//
// ListStartOffset = border(1) + padding(1) + title(1) + space(1) + input(1) +
//
//	separator(1) = 6
var commandPaletteLayout = pickerLayout{
	WidthPercent:    80,
	MinWidth:        50,
	MaxWidth:        80,
	HeightPercent:   70,
	MaxHeight:       30,
	ListOverhead:    8,
	ListStartOffset: 6,
}

// NewCommandPaletteDialog creates a new command palette dialog.
func NewCommandPaletteDialog(categories []commands.Category) Dialog {
	d := &commandPaletteDialog{
		pickerCore: newPickerCore(commandPaletteLayout, "Type to search commands…"),
		categories: categories,
	}
	d.textInput.CharLimit = 100
	d.filterCommands()
	return d
}

func (d *commandPaletteDialog) Init() tea.Cmd { return textinput.Blink }

func (d *commandPaletteDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Scrollview handles mouse scrollbar, wheel, and pgup/pgdn/home/end
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		cmd := d.updateInput(msg, d.filterCommands)
		return d, cmd

	case tea.MouseClickMsg:
		// Scrollbar clicks already handled above; this handles list item clicks.
		if dbl, _ := d.handleListClick(msg, d.lineToCmdIndex); dbl {
			cmd := d.executeSelected()
			return d, cmd
		}
		return d, nil

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		switch {
		case key.Matches(msg, d.keyMap.Escape):
			return d, closeDialogCmd()
		case key.Matches(msg, d.keyMap.Up):
			d.navigate(-1, len(d.filtered), d.findSelectedLine)
			return d, nil
		case key.Matches(msg, d.keyMap.Down):
			d.navigate(+1, len(d.filtered), d.findSelectedLine)
			return d, nil
		case key.Matches(msg, d.keyMap.Enter):
			cmd := d.executeSelected()
			return d, cmd
		default:
			cmd := d.updateInput(msg, d.filterCommands)
			return d, cmd
		}
	}

	return d, nil
}

// executeSelected executes the currently selected command and closes the dialog.
func (d *commandPaletteDialog) executeSelected() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.filtered) {
		return nil
	}
	selectedCmd := d.filtered[d.selected]
	cmds := []tea.Cmd{closeDialogCmd()}
	if selectedCmd.Execute != nil {
		cmds = append(cmds, selectedCmd.Execute(""))
	}
	return tea.Sequence(cmds...)
}

// filterCommands filters the command list based on search input.
func (d *commandPaletteDialog) filterCommands() {
	query := strings.ToLower(strings.TrimSpace(d.textInput.Value()))

	d.filtered = d.filtered[:0]
	for _, cat := range d.categories {
		for _, cmd := range cat.Commands {
			if query == "" || matchesCommandQuery(cmd, query) {
				d.filtered = append(d.filtered, cmd)
			}
		}
	}

	// Clearing the search returns the cursor to the top, matching the file
	// picker. Filtered queries preserve the cursor when still in range.
	if query == "" || d.selected >= len(d.filtered) {
		d.selected = 0
	}
	d.scrollview.SetScrollOffset(0)
}

// matchesCommandQuery reports whether the given command matches the lowercase
// query string by searching label, description, or slash command. The
// category is intentionally excluded: category names act as section headers
// and matching them would surface every command in a category, drowning out
// targeted queries (e.g. typing "session" would otherwise match every
// command in the Session category).
func matchesCommandQuery(cmd commands.Item, query string) bool {
	return strings.Contains(strings.ToLower(cmd.Label), query) ||
		strings.Contains(strings.ToLower(cmd.Description), query) ||
		strings.Contains(strings.ToLower(cmd.SlashCommand), query)
}

// buildList builds the visual list of commands grouped by category, with a
// blank line + category header before each new group. Pass contentWidth=0 to
// produce a layout-only list (used by mouse hit-testing and findSelectedLine,
// which only need the line→item mapping).
func (d *commandPaletteDialog) buildList(contentWidth int) *groupedList {
	gl := newGroupedList()
	var lastCategory string

	for i, cmd := range d.filtered {
		if cmd.Category != lastCategory {
			if lastCategory != "" {
				gl.AddNonItem("")
			}
			gl.AddNonItem(d.renderCategoryHeader(cmd.Category, contentWidth))
			lastCategory = cmd.Category
		}
		gl.AddItem(d.renderCommand(cmd, i == d.selected, contentWidth))
	}

	return gl
}

func (d *commandPaletteDialog) renderCategoryHeader(category string, contentWidth int) string {
	if contentWidth <= 0 {
		return category
	}
	return styles.PaletteCategoryStyle.MarginTop(0).Render(category)
}

// lineToCmdIndex returns the command index for a rendered line, or -1
// when the line is a header or blank. Used for mouse hit-testing.
func (d *commandPaletteDialog) lineToCmdIndex(line int) int {
	return d.buildList(0).ItemForLine(line)
}

// findSelectedLine returns the rendered line index for the selected command.
func (d *commandPaletteDialog) findSelectedLine() int {
	return d.buildList(0).LineForItem(d.selected)
}

// buildLines returns the rendered lines and the line→item mapping. It exists
// for the test suite; production code uses buildList directly.
func (d *commandPaletteDialog) buildLines(contentWidth int) (lines []string, lineToCmd []int) {
	gl := d.buildList(contentWidth)
	return gl.Lines(), gl.LineToItem()
}

// View renders the command palette dialog.
func (d *commandPaletteDialog) View() string {
	dialogWidth, _, contentWidth := d.dialogSize()
	d.textInput.SetWidth(contentWidth)

	gl := d.buildList(contentWidth)
	d.updateScrollviewPosition()
	d.scrollview.SetContent(gl.Lines(), len(gl.Lines()))

	scrollableContent := d.scrollview.View()
	if len(d.filtered) == 0 {
		scrollableContent = d.renderEmptyState("No commands found", contentWidth)
	}

	content := NewContent(d.regionWidth(contentWidth)).
		AddTitle("Commands").
		AddSpace().
		AddContent(d.textInput.View()).
		AddSeparator().
		AddContent(scrollableContent).
		AddSpace().
		AddHelpKeys("↑/↓", "navigate", "enter", "execute", "esc", "close").
		Build()

	return styles.DialogStyle.Width(dialogWidth).Render(content)
}

// renderCommand renders a single command line in the list.
func (d *commandPaletteDialog) renderCommand(cmd commands.Item, selected bool, contentWidth int) string {
	if contentWidth <= 0 {
		return ""
	}

	actionStyle := styles.PaletteUnselectedActionStyle
	descStyle := styles.PaletteUnselectedDescStyle
	if selected {
		actionStyle = styles.PaletteSelectedActionStyle
		descStyle = styles.PaletteSelectedDescStyle
	}

	label := " " + cmd.Label
	content := actionStyle.Render(label)
	if cmd.Description == "" {
		return content
	}

	const separator = " • "
	availableWidth := contentWidth - lipgloss.Width(content) - lipgloss.Width(separator)
	if availableWidth <= 0 {
		return content
	}
	return content + descStyle.Render(separator+toolcommon.TruncateText(cmd.Description, availableWidth))
}
