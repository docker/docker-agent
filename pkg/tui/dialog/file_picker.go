package dialog

import (
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/docker/go-units"

	"github.com/docker/docker-agent/pkg/fsx"
	"github.com/docker/docker-agent/pkg/tui/components/toolcommon"
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
	// filePickerListOverhead = title(1) + space(1) + dir(1) + input(1) +
	// separator(1) + space(1) + help row(2) + borders/padding(3)
	filePickerListOverhead = 11
	// filePickerListStartY = border(1) + padding(1) + title(1) + space(1) +
	// dir(1) + input(1) + separator(1)
	filePickerListStartY = 7
)

// filePickerLayout is the layout used by the file picker.
var filePickerLayout = pickerLayout{
	WidthPercent:    80,
	MinWidth:        60,
	MaxWidth:        120,
	HeightPercent:   70,
	MaxHeight:       30,
	ListOverhead:    filePickerListOverhead,
	ListStartOffset: filePickerListStartY,
}

// filePickerKeyMap holds the file-picker-specific visibility toggles, on top
// of the navigation bindings provided by the shared pickerKeyMap.
//
// On macOS the OS substitutes a Unicode character for Option+key at the
// input-system level before the terminal sees the event, so the alt+h / alt+i
// ESC sequences never reach us. Each toggle therefore also accepts the
// characters that the standard US and US-International layouts emit (Option+H
// -> "˙" U+02D9, Option+I -> "ˆ" U+02C6). Option+I is a dead key on macOS, so
// the first press may arrive as an escaped or combining circumflex. See issue
// #2611. The visible help labels are rendered separately (and dynamically) by
// filePickerHelpKeysRows.
type filePickerKeyMap struct {
	ToggleHidden  key.Binding
	ToggleIgnored key.Binding
}

func defaultFilePickerKeyMap() filePickerKeyMap {
	return filePickerKeyMap{
		ToggleHidden: key.NewBinding(
			key.WithKeys("alt+h", "˙", "alt+˙"),
			key.WithHelp("alt+h", "toggle hidden"),
		),
		ToggleIgnored: key.NewBinding(
			key.WithKeys("alt+i", "ˆ", "alt+ˆ", "\u0302", "alt+\u0302"),
			key.WithHelp("alt+i", "toggle ignored"),
		),
	}
}

type filePickerDialog struct {
	pickerCore

	fpKeyMap    filePickerKeyMap
	currentDir  string
	entries     []fileEntry
	filtered    []fileEntry
	err         error
	showHidden  bool
	showIgnored bool
}

// NewFilePickerDialog creates a new file picker dialog for attaching files.
// If initialPath is provided and is a directory, it starts in that directory.
// If initialPath is a file, it starts in the file's directory with the file pre-selected.
func NewFilePickerDialog(initialPath string) Dialog {
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
		pickerCore: newPickerCore(filePickerLayout, "Type to filter files…"),
		fpKeyMap:   defaultFilePickerKeyMap(),
		currentDir: startDir,
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

	if filepath.Dir(d.currentDir) != d.currentDir {
		d.entries = append(d.entries, fileEntry{
			name:  "..",
			path:  filepath.Dir(d.currentDir),
			isDir: true,
		})
	}

	var shouldIgnore func(string) bool
	if vcsMatcher, err := fsx.NewVCSMatcher(d.currentDir); err == nil && vcsMatcher != nil {
		shouldIgnore = vcsMatcher.ShouldIgnore
	}

	dirEntries, err := os.ReadDir(d.currentDir)
	if err != nil {
		d.err = err
		return
	}

	for _, entry := range dirEntries {
		if !d.showHidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		fullPath := filepath.Join(d.currentDir, entry.Name())
		if !d.showIgnored && shouldIgnore != nil && shouldIgnore(fullPath) {
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
		if entry.IsDir() {
			continue
		}
		if !d.showHidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		fullPath := filepath.Join(d.currentDir, entry.Name())
		if !d.showIgnored && shouldIgnore != nil && shouldIgnore(fullPath) {
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

func (d *filePickerDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	// Scrollview handles mouse click/motion/release, wheel, and pgup/pgdn/home/end.
	if handled, cmd := d.scrollview.Update(msg); handled {
		return d, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		cmd := d.SetSize(msg.Width, msg.Height)
		return d, cmd

	case messages.ThemeChangedMsg:
		d.refreshInputStyles()
		return d, nil

	case tea.PasteMsg:
		cmd := d.updateInput(msg, d.filterEntries)
		return d, cmd

	case tea.KeyPressMsg:
		if cmd := HandleQuit(msg); cmd != nil {
			return d, cmd
		}
		switch {
		case key.Matches(msg, d.keyMap.Escape):
			return d, closeDialogCmd()
		case key.Matches(msg, d.keyMap.Up):
			d.navigate(-1, len(d.filtered), nil)
			return d, nil
		case key.Matches(msg, d.keyMap.Down):
			d.navigate(+1, len(d.filtered), nil)
			return d, nil
		case key.Matches(msg, d.keyMap.Enter):
			cmd := d.activateSelected()
			return d, cmd
		case key.Matches(msg, d.fpKeyMap.ToggleHidden):
			d.toggleHidden()
			return d, nil
		case key.Matches(msg, d.fpKeyMap.ToggleIgnored):
			d.toggleIgnored()
			return d, nil
		default:
			cmd := d.updateInput(msg, d.filterEntries)
			return d, cmd
		}
	}

	return d, nil
}

// activateSelected handles enter on the current entry. Directories are
// navigated into; files are returned to the caller via InsertFileRefMsg.
func (d *filePickerDialog) activateSelected() tea.Cmd {
	if d.selected < 0 || d.selected >= len(d.filtered) {
		return nil
	}
	entry := d.filtered[d.selected]
	if entry.isDir {
		d.currentDir = entry.path
		d.textInput.SetValue("")
		d.loadDirectory()
		return nil
	}
	return tea.Sequence(
		closeDialogCmd(),
		core.CmdHandler(messages.InsertFileRefMsg{FilePath: entry.path}),
	)
}

func (d *filePickerDialog) toggleHidden() {
	d.showHidden = !d.showHidden
	d.loadDirectory()
	d.filterEntries()
}

func (d *filePickerDialog) toggleIgnored() {
	d.showIgnored = !d.showIgnored
	d.loadDirectory()
	d.filterEntries()
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

// truncateDir shortens dir to fit contentWidth columns, keeping the path tail
// behind an ellipsis. Two columns are reserved for the "📁 " prefix. Embedded
// line breaks are escaped first: lipgloss.Width measures the widest line, so a
// raw newline passes the fit check and renders a second header row that the
// picker's fixed one-row scrollbar offset does not account for.
func truncateDir(dir string, contentWidth int) string {
	dir = pathEscaper.Replace(dir)
	return toolcommon.TruncateTextLeft(dir, contentWidth-4)
}

// pathEscaper renders line breaks visible instead of letting them split the
// one-line header.
var pathEscaper = strings.NewReplacer("\n", `\n`, "\r", `\r`)

func (d *filePickerDialog) View() string {
	dialogWidth, _, contentWidth := d.dialogSize()
	d.textInput.SetWidth(contentWidth)

	displayDir := truncateDir(d.currentDir, contentWidth)
	dirLine := styles.MutedStyle.Render("📁 " + displayDir)

	var allLines []string
	for i, entry := range d.filtered {
		allLines = append(allLines, d.renderEntry(entry, i == d.selected, contentWidth))
	}

	d.updateScrollviewPosition()
	d.scrollview.SetContent(allLines, len(allLines))

	var scrollableContent string
	switch {
	case d.err != nil:
		scrollableContent = d.renderErrorState(d.err.Error(), contentWidth)
	case len(d.filtered) == 0:
		scrollableContent = d.renderEmptyState("No files found", contentWidth)
	default:
		scrollableContent = d.scrollview.View()
	}

	helpRow1, helpRow2 := d.filePickerHelpKeysRows(d.regionWidth(contentWidth))
	content := NewContent(d.regionWidth(contentWidth)).
		AddTitle("Attach File").
		AddSpace().
		AddContent(dirLine).
		AddContent(d.textInput.View()).
		AddSeparator().
		AddContent(scrollableContent).
		AddSpace().
		AddHelpKeys(helpRow1...).
		AddHelpKeys(helpRow2...).
		Build()

	return styles.DialogStyle.Width(dialogWidth).Render(content)
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
	maxNameLen := max(1, maxWidth-20)
	if r := []rune(name); len(r) > maxNameLen {
		name = string(r[:maxNameLen-1]) + "…"
	}

	line := nameStyle.Render(icon + name)
	if !entry.isDir && entry.size > 0 {
		line += descStyle.Render(" " + units.HumanSize(float64(entry.size)))
	}

	return line
}

// filePickerHelpKeysRows returns the help key bindings split into two rows.
// When contentWidth is wide enough to fit every shortcut on a single line,
// all bindings are returned in row1 and row2 is empty; the second (empty) row
// keeps the dialog height stable.
func (d *filePickerDialog) filePickerHelpKeysRows(contentWidth int) (row1, row2 []string) {
	hiddenLabel := "show hidden"
	if d.showHidden {
		hiddenLabel = "hide hidden"
	}
	ignoredLabel := "show ignored"
	if d.showIgnored {
		ignoredLabel = "hide ignored"
	}
	all := []string{
		"↑/↓", "navigate",
		"enter", "select",
		"esc", "close",
		"alt+h", hiddenLabel,
		"alt+i", ignoredLabel,
	}
	if HelpKeysWidth(all...) <= contentWidth {
		return all, nil
	}
	return all[:8], all[8:]
}
