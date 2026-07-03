package dialog

import (
	"os"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

const (
	contextDialogWidthPercent = 70
	contextDialogMinWidth     = 50
	contextDialogMaxWidth     = 100
)

// ContextFile is one entry in the /context inventory dialog.
type ContextFile struct {
	// Path is the absolute path of the file.
	Path string
	// Prompt marks files injected via the agent's add_prompt_files config;
	// they come from the agent configuration and cannot be dropped.
	Prompt bool
	// Tokens is the approximate token count of the file, or -1 when the
	// file could not be stat-ed (e.g. deleted since it was attached).
	Tokens int64
}

// BuildContextFiles builds the /context dialog entries: attached files first,
// then prompt files, each with a token estimate derived from its on-disk size.
func BuildContextFiles(attached, promptFiles []string) []ContextFile {
	files := make([]ContextFile, 0, len(attached)+len(promptFiles))
	for _, p := range attached {
		files = append(files, ContextFile{Path: p, Tokens: approxFileTokens(p)})
	}
	for _, p := range promptFiles {
		files = append(files, ContextFile{Path: p, Prompt: true, Tokens: approxFileTokens(p)})
	}
	return files
}

// approxFileTokens estimates a file's token count from its size using the
// same ~4 chars/token rule of thumb the session uses for truncation budgets.
// Returns -1 when the file cannot be stat-ed or is not a regular file.
func approxFileTokens(path string) int64 {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return -1
	}
	return info.Size() / 4
}

// contextDialog lists every file currently entering the session context
// (attachments and prompt files) and lets the user drop attachments.
type contextDialog struct {
	BaseDialog

	files    []ContextFile
	selected int
}

// NewContextDialog creates the /context dialog. files must contain attached
// files first, then prompt files (the order BuildContextFiles produces).
func NewContextDialog(files []ContextFile) Dialog {
	return &contextDialog{files: files}
}

func (d *contextDialog) Init() tea.Cmd { return nil }

func (d *contextDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		cmd := d.handleKey(msg)
		return d, cmd
	}
	return d, nil
}

func (d *contextDialog) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "q", "enter":
		return core.CmdHandler(CloseDialogMsg{})
	case "up", "k":
		if d.selected > 0 {
			d.selected--
		}
	case "down", "j":
		if d.selected < len(d.files)-1 {
			d.selected++
		}
	case "home", "g":
		d.selected = 0
	case "end", "G":
		d.selected = max(0, len(d.files)-1)
	case "d", "x", "delete", "backspace":
		return d.dropSelected()
	}
	return nil
}

// dropSelected removes the selected attachment from the dialog's local list
// and asks the app to remove it from the session. Prompt files cannot be
// dropped: they are re-resolved from the agent configuration at every turn.
func (d *contextDialog) dropSelected() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.files) {
		return nil
	}
	file := d.files[d.selected]
	if file.Prompt {
		return notification.InfoCmd("Prompt files come from the agent configuration and cannot be dropped")
	}
	d.files = append(d.files[:d.selected], d.files[d.selected+1:]...)
	if d.selected >= len(d.files) {
		d.selected = max(0, len(d.files)-1)
	}
	return core.CmdHandler(messages.DropAttachedFileMsg{FilePath: file.Path})
}

func (d *contextDialog) Position() (row, col int) {
	return d.CenterDialog(d.View())
}

func (d *contextDialog) View() string {
	width := d.ComputeDialogWidth(contextDialogWidthPercent, contextDialogMinWidth, contextDialogMaxWidth)
	inner := d.ContentWidth(width, 2)

	content := NewContent(inner).AddTitle("Context Files").AddSeparator().AddSpace()

	if summary := d.summaryLine(); summary != "" {
		content = content.
			AddContent(styles.DialogOptionsStyle.Width(inner).Render(summary)).
			AddSpace()
	}

	body := content.
		AddContent(d.bodyContent(inner)).
		AddSpace().
		AddHelpKeys(d.helpKeys()...).
		Build()

	return styles.DialogStyle.Width(width).Render(body)
}

// summaryLine returns the "N attached • M prompt files • ~X tokens" header,
// or "" when there are no files.
func (d *contextDialog) summaryLine() string {
	attached, prompts, tokens := 0, 0, int64(0)
	for _, f := range d.files {
		if f.Prompt {
			prompts++
		} else {
			attached++
		}
		if f.Tokens > 0 {
			tokens += f.Tokens
		}
	}
	if attached+prompts == 0 {
		return ""
	}

	summary := pluralize(attached, "attached file", "attached files")
	if prompts > 0 {
		summary += "  •  " + pluralize(prompts, "prompt file", "prompt files")
	}
	if tokens > 0 {
		summary += "  •  ~" + toolcommon.FormatTokenCount(tokens) + " tokens"
	}
	return summary
}

// bodyContent returns either the empty-state line or the grouped file list.
func (d *contextDialog) bodyContent(inner int) string {
	if len(d.files) == 0 {
		return styles.DialogContentStyle.
			Italic(true).
			Foreground(styles.TextMuted).
			Width(inner).
			Align(lipgloss.Center).
			Render("No files in context. Attach files with @path or /attach.")
	}

	gl := newGroupedList()
	prevPrompt := -1 // tri-state: -1 = no group yet, 0 = attached, 1 = prompt
	for i, f := range d.files {
		group := 0
		if f.Prompt {
			group = 1
		}
		if group != prevPrompt {
			if group == 0 {
				gl.AddNonItem(RenderGroupSeparator("Attached files", inner))
			} else {
				gl.AddNonItem(RenderGroupSeparator("Prompt files (agent config)", inner))
			}
			prevPrompt = group
		}
		gl.AddItem(d.renderRow(f, i == d.selected, inner))
	}
	return lipgloss.JoinVertical(lipgloss.Left, d.visibleLines(gl)...)
}

// visibleLines applies a sliding window over the rendered lines so that long
// file lists fit the screen while the selected item stays visible.
func (d *contextDialog) visibleLines(gl *groupedList) []string {
	lines := gl.Lines()
	maxVisible := d.maxVisibleLines()
	if len(lines) <= maxVisible {
		return lines
	}

	selectedLine := gl.LineForItem(d.selected)
	start := min(max(0, selectedLine-maxVisible/2), len(lines)-maxVisible)
	return lines[start : start+maxVisible]
}

// maxVisibleLines returns how many list lines fit in the dialog, leaving
// room for the frame, title, separator, summary, and help rows.
func (d *contextDialog) maxVisibleLines() int {
	const chromeLines = 10
	return max(3, d.Height()*70/100-chromeLines)
}

func (d *contextDialog) helpKeys() []string {
	if len(d.files) == 0 {
		return []string{"esc", "close"}
	}
	return []string{"↑/↓", "navigate", "d", "drop", "esc", "close"}
}

// renderRow draws one file entry: the display path on the left and the
// token estimate right-aligned.
func (d *contextDialog) renderRow(f ContextFile, selected bool, width int) string {
	nameStyle, descStyle := styles.PaletteUnselectedActionStyle, styles.PaletteUnselectedDescStyle
	if selected {
		nameStyle, descStyle = styles.PaletteSelectedActionStyle, styles.PaletteSelectedDescStyle
	}

	tokens := "missing"
	if f.Tokens >= 0 {
		tokens = "~" + toolcommon.FormatTokenCount(f.Tokens) + " tokens"
	}

	right := descStyle.Render(" " + tokens + " ")
	maxPathWidth := width - lipgloss.Width(right) - 2
	left := nameStyle.Render(" " + toolcommon.TruncateText(displayContextPath(f.Path), maxPathWidth) + " ")
	gap := max(0, width-lipgloss.Width(left))
	return left + lipgloss.PlaceHorizontal(gap, lipgloss.Right, right,
		lipgloss.WithWhitespaceStyle(descStyle))
}

// displayContextPath shortens an absolute path for display: relative to the
// current working directory when inside it, otherwise with ~ for the home
// directory.
func displayContextPath(p string) string {
	if cwd, err := os.Getwd(); err == nil && pathx.IsWithin(p, cwd) {
		return pathx.RelativeTo(p, cwd)
	}
	return pathx.ShortenHome(p)
}
