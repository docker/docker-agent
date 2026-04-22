package dialog

import (
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/docker/go-units"

	"github.com/docker/docker-agent/pkg/fsx"
	"github.com/docker/docker-agent/pkg/tui/components/scrollview"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

type fileEntry struct {
	name  string
	path  string
	isDir bool
	size  int64
}

const (
	filePickerListOverhead = 10
	filePickerListStartY   = 7 // border(1) + padding(1) + title(1) + space(1) + dir(1) + input(1) + separator(1)
)

type filePickerDialog struct {
	BaseDialog

	textInput      textinput.Model
	currentDir     string
	entries        []fileEntry
	filtered       []fileEntry
	selected       int
	scrollview     *scrollview.Model
	keyMap         commandPaletteKeyMap
	err            error
	showGitignored bool
}

// NewFilePickerDialog creates a new file picker dialog for attaching files.
// If initialPath is provided and is a directory, it starts in that directory.
// If initialPath is a file, it starts in the file's directory with the file pre-selected.
// If showGitignored is true, gitignored files will be shown in the picker.
func NewFilePickerDialog(initialPath string, showGitignored bool) Dialog {
	ti := textinput.New()
	ti.Placeholder = "Type to filter files…"
	ti.Focus()
	ti.CharLimit = 256
	ti.SetWidth(50)

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	startDir := cwd
	var selectFile string

	if initialPath != "" {
		if !filepath.IsAbs(initialPath) {
			initialPath = filepath.Join(cwd, initialPath)
		}

		info, err := os.Stat(initialPath)
		if err == nil {
			if info.IsDir() {
				startDir = initialPath
			} else {
				startDir = filepath.Dir(initialPath)
				selectFile = filepath.Base(initialPath)
			}
		} else {
			parentDir := filepath.Dir(initialPath)
			if info, err := os.Stat(parentDir); err == nil && info.IsDir() {
				startDir = parentDir
			}
		}
	}

	d := &filePickerDialog{
		textInput:      ti,
		currentDir:     startDir,
		scrollview:     scrollview.New(scrollview.WithReserveScrollbarSpace(true)),
		keyMap:         defaultCommandPaletteKeyMap(),
		showGitignored: showGitignored,
	}

	d.loadDirectory()

	if selectFile != "" {
		for i, entry := range d.filtered {
			if entry.name == selectFile {
				d.selected = i
				break
			}
		}
	}

	return d
}

func (d *filePickerDialog) loadDirectory() {
	d.entries = nil
	d.filtered = nil
	d.selected = 0
	d.scrollview.SetScrollOffset(0)
	d.err = nil

	if d.currentDir != "/" {
		d.entries = append(d.entries, fileEntry{
			name:  "..",
			path:  filepath.Dir(d.currentDir),
			isDir: true,
		})
	}

	var shouldIgnore func(string) bool
	if !d.showGitignored {
		if vcsMatcher, err := fsx.NewVCSMatcher(d.currentDir); err == nil && vcsMatcher != nil {
			shouldIgnore = vcsMatcher.ShouldIgnore
		}
	}

	dirEntries, err := os.ReadDir(d.currentDir)
	if err != nil {
		d.err = err
		return
	}

	for _, entry := range dirEntries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		fullPath := filepath.Join(d.currentDir, entry.Name())
		if shouldIgnore != nil && shouldIgnore(fullPath) {
			continue
		}
		if entry.IsDir() {
			d.entries = append(d.entries, fileEntry{
				name:  entry.Name() + "/",
				path:  fullPath,
				isDir: true,
			})
		}
	}

	for _, entry := range dirEntries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		fullPath := filepath.Join(d.currentDir, entry.Name())
		if shouldIgnore != nil && shouldIgnore(fullPath) {
			continue
		}
		info, err := entry.Info()
		size := int64(0)
		if err == nil {
			size = info.Size()
		}
		d.entries = append(d.entries, fileEntry{
			name:  entry.Name(),
			path:  fullPath,
			isDir: false,
			size:  size,
		})
	}

	d.filtered = d.entries
}

func (d *filePickerDialog) Init() tea.Cmd {
	return textinput.Blink
}

func (d *filePickerDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Scrollview handles mouse click/motion/release, wheel, and pgup/pgdn/home/end
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case tea.PasteMsg:
		var cmd tea.Cmd
		d.textInput, cmd = d.textInput.Update(msg)
		d.filterEntries()
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}

		switch {
		case key.Matches(msg, d.keyMap.Escape):
			return d, core.CmdHandler(CloseDialogMsg{})

		case key.Matches(msg, d.keyMap.Up):
			if d.selected > 0 {
				d.selected--
				d.scrollview.EnsureLineVisible(d.selected)
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Down):
			if d.selected < len(d.filtered)-1 {
				d.selected++
				d.scrollview.EnsureLineVisible(d.selected)
			}
			return d, nil

		case key.Matches(msg, d.keyMap.Enter):
			if d.selected >= 0 && d.selected < len(d.filtered) {
				entry := d.filtered[d.selected]
				if entry.isDir {
					d.currentDir = entry.path
					d.textInput.SetValue("")
					d.loadDirectory()
					return d, nil
				}
				return d, tea.Sequence(
					core.CmdHandler(CloseDialogMsg{}),
					core.CmdHandler(messages.InsertFileRefMsg{FilePath: entry.path}),
				)
			}
			return d, nil

		default:
			var cmd tea.Cmd
			d.textInput, cmd = d.textInput.Update(msg)
			d.filterEntries()
			return d, cmd
		}
	}

	return d, nil
}

func (d *filePickerDialog) filterEntries() {
	query := strings.ToLower(strings.TrimSpace(d.textInput.Value()))
	if query == "" {
		d.filtered = d.entries
		d.selected = 0
		d.scrollview.SetScrollOffset(0)
		return
	}

	d.filtered = nil
	for _, entry := range d.entries {
		if entry.name == ".." {
			d.filtered = append(d.filtered, entry)
			continue
		}
		if strings.Contains(strings.ToLower(entry.name), query) {
			d.filtered = append(d.filtered, entry)
		}
	}

	if d.selected >= len(d.filtered) {
		d.selected = 0
	}
	d.scrollview.SetScrollOffset(0)
}

func (d *filePickerDialog) dialogSize() (dialogWidth, maxHeight, contentWidth int) {
	dialogWidth = max(min(d.Width()*80/100, 80), 60)
	maxHeight = min(d.Height()*70/100, 30)
	contentWidth = dialogWidth - 6 - d.scrollview.ReservedCols()
	return dialogWidth, maxHeight, contentWidth
}

func (d *filePickerDialog) View() string {
	dialogWidth, _, contentWidth := d.dialogSize()
	d.textInput.SetWidth(contentWidth)

	displayDir := d.currentDir
	if len(displayDir) > contentWidth-4 {
		displayDir = "…" + displayDir[len(displayDir)-(contentWidth-5):]
	}
	dirLine := styles.MutedStyle.Render("📁 " + displayDir)

	// Build all entry lines
	var allLines []string
	for i, entry := range d.filtered {
		allLines = append(allLines, d.renderEntry(entry, i == d.selected, contentWidth))
	}

	regionWidth := contentWidth + d.scrollview.ReservedCols()
	visibleLines := d.scrollview.VisibleHeight()

	// Set scrollview position for mouse hit-testing (auto-computed from dialog position)
	dialogRow, dialogCol := d.Position()
	d.scrollview.SetPosition(dialogCol+3, dialogRow+filePickerListStartY)

	d.scrollview.SetContent(allLines, len(allLines))

	var scrollableContent string
	switch {
	case d.err != nil:
		errLines := []string{"", styles.ErrorStyle.
			Align(lipgloss.Center).Width(contentWidth).
			Render(d.err.Error())}
		for len(errLines) < visibleLines {
			errLines = append(errLines, "")
		}
		scrollableContent = d.scrollview.ViewWithLines(errLines)
	case len(d.filtered) == 0:
		emptyLines := []string{"", styles.DialogContentStyle.
			Italic(true).Align(lipgloss.Center).Width(contentWidth).
			Render("No files found")}
		for len(emptyLines) < visibleLines {
			emptyLines = append(emptyLines, "")
		}
		scrollableContent = d.scrollview.ViewWithLines(emptyLines)
	default:
		scrollableContent = d.scrollview.View()
	}

	content := NewContent(regionWidth).
		AddTitle("Attach File").
		AddSpace().
		AddContent(dirLine).
		AddContent(d.textInput.View()).
		AddSeparator().
		AddContent(scrollableContent).
		AddSpace().
		AddHelpKeys("↑/↓", "navigate", "enter", "select", "esc", "close").
		Build()

	return styles.DialogStyle.Width(dialogWidth).Render(content)
}

// SetSize sets the dialog dimensions and configures the scrollview region.
func (d *filePickerDialog) SetSize(width, height int) tea.Cmd {
	cmd := d.BaseDialog.SetSize(width, height)
	_, maxHeight, contentWidth := d.dialogSize()
	regionWidth := contentWidth + d.scrollview.ReservedCols()
	visibleLines := max(1, maxHeight-filePickerListOverhead)
	d.scrollview.SetSize(regionWidth, visibleLines)
	return cmd
}

func (d *filePickerDialog) renderEntry(entry fileEntry, selected bool, maxWidth int) string {
	nameStyle, descStyle := styles.PaletteUnselectedActionStyle, styles.PaletteUnselectedDescStyle
	if selected {
		nameStyle, descStyle = styles.PaletteSelectedActionStyle, styles.PaletteSelectedDescStyle
	}

	var icon string
	if entry.isDir {
		icon = "📁 "
	} else {
		icon = "📄 "
	}

	name := entry.name
	maxNameLen := maxWidth - 20
	if len(name) > maxNameLen {
		name = name[:maxNameLen-1] + "…"
	}

	line := nameStyle.Render(icon + name)
	if !entry.isDir && entry.size > 0 {
		line += descStyle.Render(" " + units.HumanSize(float64(entry.size)))
	}

	return line
}

func (d *filePickerDialog) Position() (row, col int) {
	dialogWidth, maxHeight, _ := d.dialogSize()
	return CenterPosition(d.Width(), d.Height(), dialogWidth, maxHeight)
}
