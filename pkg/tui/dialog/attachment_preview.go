package dialog

import (
	"cmp"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/components/editor"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Dialog sizing constants
const (
	dialogSizePercent  = 70 // dialog uses 70% of terminal dimensions
	dialogFramePadding = 6  // border (2) + internal padding (4)
	dialogMinWidth     = 20
	dialogChromeRows   = 4 // title + separator + blank line + help
	dialogFrameHeight  = 4 // top/bottom border + padding
	minViewportHeight  = 5
	tabWidth           = 4
)

type attachmentPreviewDialog struct {
	BaseDialog

	preview  editor.AttachmentPreview
	viewport viewport.Model

	titleView     string
	separatorView string
	helpView      string
	dialogWidth   int
	dialogHeight  int
	innerWidth    int
}

// NewAttachmentPreviewDialog returns a dialog that shows attachment content in a scrollable view.
func NewAttachmentPreviewDialog(preview editor.AttachmentPreview) Dialog {
	vp := viewport.New(
		viewport.WithWidth(80),
		viewport.WithHeight(20),
	)
	vp.SoftWrap = true
	vp.FillHeight = true
	vp.LeftGutterFunc = func(ctx viewport.GutterContext) string {
		str := fmt.Sprintf("%4d ", ctx.Index+1)
		if ctx.Soft {
			return styles.LineNumberStyle.Render(strings.Repeat(" ", len(str)))
		}
		return styles.LineNumberStyle.Render(str)
	}

	vp.SetContent(sanitizeContent(preview.Content))

	return &attachmentPreviewDialog{
		preview:  preview,
		viewport: vp,
	}
}

func (d *attachmentPreviewDialog) Init() tea.Cmd {
	return nil
}

func (d *attachmentPreviewDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc", "q":
			return d, core.CmdHandler(CloseDialogMsg{})
		}
	}

	var cmd tea.Cmd
	d.viewport, cmd = d.viewport.Update(msg)
	return d, cmd
}

func (d *attachmentPreviewDialog) View() string {
	// Constrain viewport output to fixed dimensions to prevent layout shifts
	viewportView := lipgloss.NewStyle().
		Height(d.viewport.Height()).
		MaxHeight(d.viewport.Height()).
		Render(d.viewport.View())

	content := lipgloss.JoinVertical(
		lipgloss.Left,
		d.titleView,
		d.separatorView,
		viewportView,
		"",
		d.helpView,
	)

	return styles.DialogStyle.
		Width(d.dialogWidth).
		Height(d.dialogHeight).
		Render(content)
}

func (d *attachmentPreviewDialog) Position() (row, col int) {
	// Use pre-computed dimensions for stable positioning
	dialogHeight := cmp.Or(d.dialogHeight, 20) // fallback before SetSize is called
	dialogWidth := cmp.Or(d.dialogWidth, dialogMinWidth)
	return CenterPosition(d.Width(), d.Height(), dialogWidth, dialogHeight)
}

func (d *attachmentPreviewDialog) SetSize(width, height int) tea.Cmd {
	d.BaseDialog.SetSize(width, height)

	// Cache computed dimensions
	d.dialogWidth = d.computeDialogWidth()
	// Reduce innerWidth by extra margin (2) to prevent softwrap/border overlap issues
	// and ensure there's a safe gap between content and dialog border
	d.innerWidth = max(dialogMinWidth, d.dialogWidth-dialogFramePadding-2)

	maxDialogHeight := max(10, (height*dialogSizePercent)/100)
	chromeHeight := dialogChromeRows + dialogFrameHeight
	viewportHeight := max(minViewportHeight, maxDialogHeight-chromeHeight)
	d.dialogHeight = chromeHeight + viewportHeight

	// Pre-render chrome elements
	d.titleView = renderSingleLine(styles.DialogTitleInfoStyle, d.preview.Title, d.innerWidth)
	d.separatorView = RenderSeparator(d.innerWidth)

	helpText := "[esc/q] close | scroll: ↑↓ / wheel"
	d.helpView = renderSingleLine(styles.DialogHelpStyle, helpText, d.innerWidth)

	d.viewport.SetWidth(d.innerWidth)
	d.viewport.SetHeight(viewportHeight)

	d.viewport.SetContent(sanitizeContent(d.preview.Content))

	return nil
}

// sanitizeContent normalizes line endings and expands tabs to spaces to prevent layout issues
// This ensures more consistent width calculations
// e.g. '/t' is counted as one char but rendered as multiple, which can cause layout issues
func sanitizeContent(content string) string {
	// Normalize line endings to \n to prevent layout issues
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	// Expand tabs to spaces to ensure soft wrap calculations match visual width
	content = strings.ReplaceAll(content, "\t", strings.Repeat(" ", tabWidth))
	return content
}

func (d *attachmentPreviewDialog) computeDialogWidth() int {
	width := d.Width() * dialogSizePercent / 100
	if width < 40 {
		width = d.Width() - 4
	}
	return max(dialogMinWidth, width)
}

func renderSingleLine(style lipgloss.Style, text string, width int) string {
	if width <= 0 {
		return ""
	}
	trimmed := ansi.Truncate(text, width, "…")
	padded := trimmed + strings.Repeat(" ", max(0, width-lipgloss.Width(trimmed)))
	return style.Width(width).Render(padded)
}
