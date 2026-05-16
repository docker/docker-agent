package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	mcptools "github.com/docker/docker-agent/pkg/tools/mcp"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// MCPPromptInputDialog implements Dialog for collecting MCP prompt parameters
type MCPPromptInputDialog struct {
	BaseDialog

	promptName   string
	promptInfo   mcptools.PromptInfo
	inputs       []textinput.Model
	arguments    []mcptools.PromptArgument
	currentInput int
	keyMap       mcpPromptInputKeyMap
}

// mcpPromptInputKeyMap defines key bindings for the MCP prompt input dialog
type mcpPromptInputKeyMap struct {
	Up     key.Binding
	Down   key.Binding
	Enter  key.Binding
	Escape key.Binding
	Tab    key.Binding
}

// defaultMCPPromptInputKeyMap returns default key bindings
func defaultMCPPromptInputKeyMap() mcpPromptInputKeyMap {
	return mcpPromptInputKeyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "shift+tab"),
			key.WithHelp("↑/shift+tab", "previous field"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "tab"),
			key.WithHelp("↓/tab", "next field"),
		),
		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "execute"),
		),
		Escape: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "cancel"),
		),
		Tab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "next field"),
		),
	}
}

// NewMCPPromptInputDialog creates a new MCP prompt input dialog
func NewMCPPromptInputDialog(promptName string, promptInfo mcptools.PromptInfo) Dialog {
	// Create text inputs for all arguments (both required and optional)
	var inputs []textinput.Model
	var arguments []mcptools.PromptArgument

	for _, arg := range promptInfo.Arguments {
		ti := textinput.New()
		ti.SetStyles(styles.DialogInputStyle)
		ti.Placeholder = arg.Description
		ti.CharLimit = 500
		ti.SetWidth(50)

		inputs = append(inputs, ti)
		arguments = append(arguments, arg)
	}

	// Focus the first input if any
	if len(inputs) > 0 {
		inputs[0].Focus()
	}

	return &MCPPromptInputDialog{
		promptName:   promptName,
		promptInfo:   promptInfo,
		inputs:       inputs,
		arguments:    arguments,
		currentInput: 0,
		keyMap:       defaultMCPPromptInputKeyMap(),
	}
}

// Init initializes the MCP prompt input dialog
func (d *MCPPromptInputDialog) Init() tea.Cmd {
	return textinput.Blink
}

// Update handles messages for the MCP prompt input dialog
func (d *MCPPromptInputDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		// Forward paste to current text input
		if d.currentInput < len(d.inputs) {
			var cmd tea.Cmd
			d.inputs[d.currentInput], cmd = d.inputs[d.currentInput].Update(msg)
			cmds = append(cmds, cmd)
		}
		return d, tea.Batch(cmds...)

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
		case key.Matches(msg, d.keyMap.Escape):
			return d, core.CmdHandler(CloseDialogMsg{})

		case key.Matches(msg, d.keyMap.Up):
			if d.currentInput > 0 {
				d.inputs[d.currentInput].Blur()
				d.currentInput--
				d.inputs[d.currentInput].Focus()
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Down), key.Matches(msg, d.keyMap.Tab):
			if d.currentInput < len(d.inputs)-1 {
				d.inputs[d.currentInput].Blur()
				d.currentInput++
				d.inputs[d.currentInput].Focus()
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Enter):
			// Collect all input values
			arguments := make(map[string]string)
			for i, input := range d.inputs {
				arguments[d.arguments[i].Name] = strings.TrimSpace(input.Value())
			}

			// Check if all required fields are filled
			allFilled := true
			for i, arg := range d.arguments {
				if arg.Required && strings.TrimSpace(d.inputs[i].Value()) == "" {
					allFilled = false
					break
				}
			}

			if allFilled {
				cmds = append(cmds,
					core.CmdHandler(CloseDialogMsg{}),
					core.CmdHandler(messages.MCPPromptMsg{
						PromptName: d.promptName,
						Arguments:  arguments,
					}),
				)
				return d, tea.Sequence(cmds...)
			}
			return d, nil

		default:
			// Update the current input
			if d.currentInput < len(d.inputs) {
				var cmd tea.Cmd
				d.inputs[d.currentInput], cmd = d.inputs[d.currentInput].Update(msg)
				cmds = append(cmds, cmd)
			}
		}
	}

	return d, tea.Batch(cmds...)
}

// View renders the MCP prompt input dialog
func (d *MCPPromptInputDialog) View() string {
	dialogWidth, contentWidth := d.mcpPromptDialogDimensions()

	title := RenderTitle("MCP Prompt: "+d.promptName, contentWidth, styles.DialogTitleStyle)

	description := ""
	if d.promptInfo.Description != "" {
		description = styles.DialogContentStyle.
			Width(contentWidth).
			Render(d.promptInfo.Description)
	}

	separator := RenderSeparator(contentWidth)

	var inputsList []string

	if len(d.inputs) == 0 {
		inputsList = append(inputsList, styles.DialogContentStyle.
			Italic(true).
			Align(lipgloss.Center).
			Width(contentWidth).
			Render("No required parameters"))
	} else {
		for i, input := range d.inputs {
			arg := d.arguments[i]

			label := arg.Name
			if arg.Required {
				label += " *"
			}

			labelStyle := styles.DialogContentStyle
			if i == d.currentInput {
				labelStyle = labelStyle.Bold(true)
			}

			inputsList = append(inputsList, labelStyle.Render(label))
			input.SetWidth(contentWidth)
			inputsList = append(inputsList, input.View())

			if i < len(d.inputs)-1 {
				inputsList = append(inputsList, "")
			}
		}
	}

	help := RenderHelpKeys(contentWidth, "↑/↓", "navigate", "enter", "execute", "esc", "cancel")

	parts := []string{title}
	if description != "" {
		parts = append(parts, "", description)
	}
	parts = append(parts, separator)
	parts = append(parts, inputsList...)
	parts = append(parts, "", help)

	return styles.DialogStyle.
		Width(dialogWidth).
		Render(lipgloss.JoinVertical(lipgloss.Left, parts...))
}

// mcpPromptDialogDimensions returns the dialog width and content width.
func (d *MCPPromptInputDialog) mcpPromptDialogDimensions() (dialogWidth, contentWidth int) {
	dialogWidth = max(min(d.Width()*80/100, 80), 60)
	contentWidth = dialogWidth - styles.DialogStyle.GetHorizontalFrameSize()
	return dialogWidth, contentWidth
}

// handleMouseClick handles mouse clicks to focus input fields.
func (d *MCPPromptInputDialog) handleMouseClick(msg tea.MouseClickMsg) (layout.Model, tea.Cmd) {
	if len(d.inputs) == 0 {
		return d, nil
	}

	dialogRow, _ := d.Position()
	_, contentWidth := d.mcpPromptDialogDimensions()

	// Compute the Y offset where fields start by measuring the rendered header.
	var headerParts []string
	headerParts = append(headerParts, RenderTitle("MCP Prompt: "+d.promptName, contentWidth, styles.DialogTitleStyle))
	if d.promptInfo.Description != "" {
		headerParts = append(headerParts, "", styles.DialogContentStyle.Width(contentWidth).Render(d.promptInfo.Description))
	}
	headerParts = append(headerParts, RenderSeparator(contentWidth))
	y := ContentStartRow(dialogRow, lipgloss.JoinVertical(lipgloss.Left, headerParts...))

	clickY := msg.Y
	for i := range d.inputs {
		// Click on label or input line focuses the field
		if clickY == y || clickY == y+1 {
			d.inputs[d.currentInput].Blur()
			d.currentInput = i
			d.inputs[d.currentInput].Focus()
			return d, nil
		}
		y += 2 // label + input

		if i < len(d.inputs)-1 {
			y++ // space between fields
		}
	}

	return d, nil
}

// Position calculates the position to center the dialog
func (d *MCPPromptInputDialog) Position() (row, col int) {
	dialogWidth := max(min(d.Width()*80/100, 80), 60)
	dialogHeight := 15 + len(d.inputs)*3 // Approximate height
	return CenterPosition(d.Width(), d.Height(), dialogWidth, dialogHeight)
}
