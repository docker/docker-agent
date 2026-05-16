package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// readOnlyScrollDialogSize defines the sizing parameters for a read-only scroll dialog.
type readOnlyScrollDialogSize struct {
	widthPercent  int
	minWidth      int
	maxWidth      int
	heightPercent int
	heightMax     int
}

// contentRenderer renders dialog content lines given the available width and max height.
type contentRenderer func(contentWidth, maxHeight int) []string

// readOnlyScrollDialog is a base for simple read-only dialogs with scrollable content.
// It handles Init, Update (scrollview + close key), Position, View, and scrolling.
// Concrete dialogs embed it and provide a contentRenderer and help key bindings.
type readOnlyScrollDialog struct {
	BaseDialog

	scrollview *scrollview.Model
	closeKey   key.Binding
	size       readOnlyScrollDialogSize
	render     contentRenderer
	helpKeys   []string // pairs of [key, description] for the footer
}

// Dialog chrome: border (top+bottom=2) + padding (top+bottom=2).
const dialogChrome = 4

// Fixed lines outside the scrollable region: header (title + separator + space) + footer (space + help).
const fixedLines = 5

// newReadOnlyScrollDialog creates a new read-only scrollable dialog.
func newReadOnlyScrollDialog(
	size readOnlyScrollDialogSize,
	render contentRenderer,
) readOnlyScrollDialog {
	return readOnlyScrollDialog{
		scrollview: scrollview.New(
			scrollview.WithKeyMap(scrollview.ReadOnlyScrollKeyMap()),
			scrollview.WithReserveScrollbarSpace(true),
		),
		closeKey: key.NewBinding(key.WithKeys("esc", "enter", "q"), key.WithHelp("Esc", "close")),
		size:     size,
		render:   render,
		helpKeys: []string{"↑↓", "scroll", "Esc", "close"},
	}
}

func (d *readOnlyScrollDialog) Init() tea.Cmd {
	return nil
}

func (d *readOnlyScrollDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		if key.Matches(msg, d.closeKey) {
			return d, core.CmdHandler(CloseDialogMsg{})
		}
	}
	return d, nil
}

func (d *readOnlyScrollDialog) dialogWidth() (dialogWidth, contentWidth int) {
	s := d.size
	dialogWidth = d.ComputeDialogWidth(s.widthPercent, s.minWidth, s.maxWidth)
	contentWidth = d.ContentWidth(dialogWidth, 2) - d.scrollview.ReservedCols()
	return dialogWidth, contentWidth
}

// maxViewport returns the maximum number of scrollable lines that fit.
func (d *readOnlyScrollDialog) maxViewport() int {
	s := d.size
	maxHeight := min(d.Height()*s.heightPercent/100, s.heightMax)
	return max(1, maxHeight-fixedLines-dialogChrome)
}

// dialogHeight computes the actual dialog height based on content and viewport.
func (d *readOnlyScrollDialog) dialogHeight(contentLineCount int) int {
	s := d.size
	maxHeight := min(d.Height()*s.heightPercent/100, s.heightMax)
	needed := contentLineCount + fixedLines + dialogChrome
	return min(needed, maxHeight)
}

func (d *readOnlyScrollDialog) Position() (row, col int) {
	dw, _ := d.dialogWidth()
	// Use max possible height for stable centering.
	s := d.size
	maxHeight := min(d.Height()*s.heightPercent/100, s.heightMax)
	return CenterPosition(d.Width(), d.Height(), dw, maxHeight)
}

func (d *readOnlyScrollDialog) View() string {
	dialogWidth, contentWidth := d.dialogWidth()
	maxViewport := d.maxViewport()
	allLines := d.render(contentWidth, maxViewport)

	const headerLines = 3 // title + separator + space
	contentLines := allLines[headerLines:]

	// Viewport: show all content if it fits, otherwise cap at maxViewport.
	viewport := min(len(contentLines), maxViewport)

	regionWidth := contentWidth + d.scrollview.ReservedCols()
	d.scrollview.SetSize(regionWidth, viewport)

	dialogRow, dialogCol := d.Position()
	d.scrollview.SetPosition(dialogCol+3, dialogRow+2+headerLines)
	d.scrollview.SetContent(contentLines, len(contentLines))

	// Use ViewWithLines to guarantee exactly `viewport` lines of output.
	scrollOut := d.scrollview.View()
	scrollOutLines := strings.Split(scrollOut, "\n")
	for len(scrollOutLines) < viewport {
		scrollOutLines = append(scrollOutLines, "")
	}
	scrollOutLines = scrollOutLines[:viewport]

	parts := make([]string, 0, headerLines+viewport+2)
	parts = append(parts, allLines[:headerLines]...)
	parts = append(parts, scrollOutLines...)
	parts = append(parts, "", RenderHelpKeys(regionWidth, d.helpKeys...))

	height := d.dialogHeight(len(contentLines))
	content := lipgloss.JoinVertical(lipgloss.Left, parts...)
	return styles.DialogStyle.Padding(1, 2).Width(dialogWidth).Height(height).MaxHeight(height).Render(content)
}
