package dialog

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// helpDialog displays all currently active key bindings in a scrollable dialog.
type helpDialog struct {
	readOnlyScrollDialog

	bindings []key.Binding
}

// NewHelpDialog creates a new help dialog that displays all active key bindings.
func NewHelpDialog(bindings []key.Binding) Dialog {
	d := &helpDialog{
		bindings: bindings,
	}
	d.readOnlyScrollDialog = newReadOnlyScrollDialog(
		readOnlyScrollDialogSize{
			widthPercent:  70,
			minWidth:      60,
			maxWidth:      100,
			heightPercent: 80,
			heightMax:     40,
		},
		d.renderContent,
	)
	d.helpKeys = []string{"↑↓", "scroll", "Esc", "close"}
	return d
}

// renderContent renders the help dialog content.
func (d *helpDialog) renderContent(contentWidth, maxHeight int) []string {
	titleStyle := styles.DialogTitleStyle
	separatorStyle := styles.DialogSeparatorStyle
	keyStyle := styles.DialogHelpStyle.Foreground(styles.TextSecondary).Bold(true)
	descStyle := styles.DialogHelpStyle

	lines := []string{
		titleStyle.Render("Active Key Bindings"),
		separatorStyle.Render(strings.Repeat("─", contentWidth)),
		"",
	}

	// Group bindings by category for better organization
	// We'll do a simple categorization based on key prefixes
	globalBindings := []key.Binding{}
	ctrlBindings := []key.Binding{}
	otherBindings := []key.Binding{}

	for _, binding := range d.bindings {
		if len(binding.Keys()) == 0 {
			continue
		}
		keyStr := binding.Keys()[0]
		switch {
		case strings.HasPrefix(keyStr, "ctrl+"):
			ctrlBindings = append(ctrlBindings, binding)
		case keyStr == "esc" || keyStr == "enter" || keyStr == "tab":
			globalBindings = append(globalBindings, binding)
		default:
			otherBindings = append(otherBindings, binding)
		}
	}

	// Render global bindings
	if len(globalBindings) > 0 {
		lines = append(lines,
			styles.DialogHelpStyle.Bold(true).Render("General"),
			"",
		)
		for _, binding := range globalBindings {
			lines = append(lines, d.formatBinding(binding, keyStyle, descStyle))
		}
		lines = append(lines, "")
	}

	// Render ctrl bindings
	if len(ctrlBindings) > 0 {
		lines = append(lines,
			styles.DialogHelpStyle.Bold(true).Render("Control Key Shortcuts"),
			"",
		)
		for _, binding := range ctrlBindings {
			lines = append(lines, d.formatBinding(binding, keyStyle, descStyle))
		}
		lines = append(lines, "")
	}

	// Render other bindings
	if len(otherBindings) > 0 {
		lines = append(lines,
			styles.DialogHelpStyle.Bold(true).Render("Other"),
			"",
		)
		for _, binding := range otherBindings {
			lines = append(lines, d.formatBinding(binding, keyStyle, descStyle))
		}
	}

	return lines
}

// formatBinding formats a single key binding as "  key  description"
func (d *helpDialog) formatBinding(binding key.Binding, keyStyle, descStyle lipgloss.Style) string {
	helpInfo := binding.Help()
	helpKey := helpInfo.Key
	helpDesc := helpInfo.Desc

	// Calculate spacing to align descriptions
	const keyWidth = 20
	const indent = 2

	keyPart := keyStyle.Render(helpKey)
	descPart := descStyle.Render(helpDesc)

	// Pad the key part to align descriptions
	keyPartWidth := lipgloss.Width(keyPart)
	padding := strings.Repeat(" ", max(1, keyWidth-keyPartWidth))

	return fmt.Sprintf("%s%s%s%s",
		strings.Repeat(" ", indent),
		keyPart,
		padding,
		descPart,
	)
}

func (d *helpDialog) Init() tea.Cmd {
	return d.readOnlyScrollDialog.Init()
}

func (d *helpDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	model, cmd := d.readOnlyScrollDialog.Update(msg)
	if rod, ok := model.(*readOnlyScrollDialog); ok {
		d.readOnlyScrollDialog = *rod
	}
	return d, cmd
}

func (d *helpDialog) View() string {
	return d.readOnlyScrollDialog.View()
}

func (d *helpDialog) Position() (int, int) {
	return d.readOnlyScrollDialog.Position()
}

func (d *helpDialog) SetSize(width, height int) tea.Cmd {
	return d.readOnlyScrollDialog.SetSize(width, height)
}

func (d *helpDialog) Bindings() []key.Binding {
	return []key.Binding{}
}
